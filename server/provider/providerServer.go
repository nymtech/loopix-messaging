// Copyright 2018 The Loopix-Messaging Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package provider implements the mix provider.
package provider

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/nymtech/directory-server/models"
	"github.com/nymtech/loopix-messaging/config"
	"github.com/nymtech/loopix-messaging/flags"
	"github.com/nymtech/loopix-messaging/helpers"
	"github.com/nymtech/loopix-messaging/logger"
	"github.com/nymtech/loopix-messaging/networker"
	"github.com/nymtech/loopix-messaging/node"
	"github.com/nymtech/loopix-messaging/sphinx"
	"github.com/sirupsen/logrus"
)

const (
	presenceInterval = 2 * time.Second

	// Below should be moved to a config file once we have it
	// logFileLocation can either point to some valid file to which all log data should be written
	// or if left an empty string, stdout will be used instead
	defaultLogFileLocation = ""
	// considering we are under heavy development and nowhere near production level, log EVERYTHING
	defaultLogLevel = "trace"
)

// ProviderIt is the interface of a given Provider mix server
type ProviderIt interface {
	networker.NetworkServer
	networker.NetworkClient
	Start() error
	GetConfig() config.MixConfig
}

// ProviderServer is the data of a Provider mix server
type ProviderServer struct {
	*node.Mix
	id              string
	host            string
	port            string
	listener        net.Listener
	assignedClients map[string]ClientRecord
	config          config.MixConfig
	haltedCh        chan struct{}
	haltOnce        sync.Once
	log             *logrus.Logger
}

// ClientRecord holds identity and network data for clients.
type ClientRecord struct {
	id     string
	host   string
	port   string
	pubKey []byte
	token  []byte
}

// Wait waits till the provider is terminated for any reason.
func (p *ProviderServer) Wait() {
	<-p.haltedCh
}

// Shutdown cleanly shuts down a given provider instance.
func (p *ProviderServer) Shutdown() {
	p.haltOnce.Do(func() { p.halt() })
}

// calls any required cleanup code
func (p *ProviderServer) halt() {
	p.log.Info("Starting graceful shutdown")
	// close any listeners, free resources, etc
	// possibly send "remove presence" message

	close(p.haltedCh)
}

// Start creates loggers for capturing info and error logs
// and starts the listening server. Returns an error
// if any operation was unsuccessful.
func (p *ProviderServer) Start() error {
	p.run()

	return nil
}

// GetConfig returns the config.MixConfig for this ProviderServer
func (p *ProviderServer) GetConfig() config.MixConfig {
	return p.config
}

// Function opens the listener to start listening on provider's host and port
func (p *ProviderServer) run() {

	defer p.listener.Close()

	go func() {
		p.log.Infof("Listening on %s", p.host+":"+p.port)
		p.listenForIncomingConnections()
	}()

	go p.startSendingPresence()

	p.Wait()
}

func (p *ProviderServer) convertRecordsToModelData() []models.RegisteredClient {
	registeredClients := make([]models.RegisteredClient, 0, len(p.assignedClients))
	for _, entry := range p.assignedClients {
		registeredClients = append(registeredClients, models.RegisteredClient{
			PubKey: base64.StdEncoding.EncodeToString(entry.pubKey),
		})
	}
	return registeredClients
}

func (p *ProviderServer) startSendingPresence() {
	ticker := time.NewTicker(presenceInterval)
	for {
		select {
		case <-ticker.C:
			if err := helpers.RegisterMixProviderPresence(p.GetPublicKey(),
				p.convertRecordsToModelData(),
				net.JoinHostPort(p.host, p.port),
			); err != nil {
				p.log.Errorf("Failed to register presence: %v", err)
			}
		case <-p.haltedCh:
			return
		}
	}
}

// Function processes the received sphinx packet, performs the
// unwrapping operation and checks whether the packet should be
// forwarded or stored. If the processing was unsuccessful and error is returned.
func (p *ProviderServer) receivedPacket(packet []byte) error {
	p.log.Infof("%s: Received new sphinx packet", p.id)

	res := p.ProcessPacket(packet)
	dePacket := res.PacketData()
	nextHop := res.NextHop()
	flag := res.Flag()
	if err := res.Err(); err != nil {
		return err
	}

	switch flag {
	case flags.RelayFlag:
		if err := p.forwardPacket(dePacket, nextHop.Address); err != nil {
			return err
		}
	case flags.LastHopFlag:
		tmpMsgID := fmt.Sprintf("TMP_MESSAGE_%v", helpers.RandomString(8))
		if err := p.storeMessage(dePacket, nextHop.Id, tmpMsgID); err != nil {
			return err
		}
	default:
		p.log.Info("Sphinx packet flag not recognised")
	}

	return nil
}

func (p *ProviderServer) forwardPacket(sphinxPacket []byte, address string) error {
	packetBytes, err := config.WrapWithFlag(flags.CommFlag, sphinxPacket)
	if err != nil {
		return err
	}
	p.log.Infof("%s: Going to forward the sphinx packet", p.id)
	err = p.send(packetBytes, address)
	if err != nil {
		return err
	}
	p.log.Infof("%s: Forwarded sphinx packet", p.id)
	return nil
}

// Function opens a connection with selected network address
// and send the passed packet. If connection failed or
// the packet could not be send, an error is returned
func (p *ProviderServer) send(packet []byte, address string) error {
	p.log.Debugf("%s: Dialling", p.id)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	p.log.Debugf("%s: Writing", p.id)

	if _, err := conn.Write(packet); err != nil {
		return err
	}
	p.log.Debugf("%s: Returning", p.id)

	return nil
}

// Function responsible for running the listening process of the server;
// The providers listener accepts incoming connections and
// passes the incoming packets to the packet handler.
// If the connection could not be accepted an error
// is logged into the log files, but the function is not stopped
func (p *ProviderServer) listenForIncomingConnections() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				p.log.Panicf("Critical accept failure: %v", err)
				return
			}
			continue
		}

		p.log.Infof("Received new connection from %s", conn.RemoteAddr())
		go p.handleConnection(conn)
	}
}

func (p *ProviderServer) replyToClient(data []byte, conn net.Conn) {
	p.log.Infof("Replying back to the client (%v)", conn.RemoteAddr())
	if _, err := conn.Write(data); err != nil {
		p.log.Errorf("Couldn't reply to the client. Connection write error: %v", err)
	}
}

func (p *ProviderServer) createClientResponse(marshalledPackets ...[]byte) ([]byte, error) {
	response := &config.ProviderResponse{
		NumberOfPackets: uint64(len(marshalledPackets)),
		Packets:         marshalledPackets,
	}
	mBytes, err := proto.Marshal(response)
	if err != nil {
		return nil, err
	}
	return mBytes, nil
}

// HandleConnection handles the received packets; it checks the flag of the
// packet and schedules a corresponding process function and returns an error.
func (p *ProviderServer) handleConnection(conn net.Conn) {
	defer func() {
		p.log.Debugf("Closing Connection to %v", conn.RemoteAddr())
		if err := conn.Close(); err != nil {
			p.log.Warnf("error when closing connection from %s: %v", conn.RemoteAddr(), err)
		}
	}()

	buff := make([]byte, 1024)
	reqLen, err := conn.Read(buff)
	if err != nil {
		p.log.Errorf("Error while reading from the connection: %v", err)
		return
	}

	var packet config.GeneralPacket
	if err = proto.Unmarshal(buff[:reqLen], &packet); err != nil {
		p.log.Errorf("Error while unmarshalling received packet: %v", err)
		return
	}

	switch flags.PacketTypeFlagFromBytes(packet.Flag) {
	case flags.AssignFlag:
		tokenBytes, err := p.handleAssignRequest(packet.Data)
		if err != nil {
			p.log.Errorf("Error while handling token request: %v", err)
			return
		}
		clientResponse, err := p.createClientResponse(tokenBytes)
		if err != nil {
			p.log.Errorf("Error while creating client response for token: %v", err)
			return
		}
		p.replyToClient(clientResponse, conn)

	case flags.CommFlag:
		if err := p.receivedPacket(packet.Data); err != nil {
			p.log.Errorf("Error while handling received packet: %v", err)
			return
		}

	case flags.PullFlag:
		messagesBytes, err := p.handlePullRequest(packet.Data)
		if err != nil {
			p.log.Errorf("Error while handling pull request: %v", err)
			return
		}

		clientResponse, err := p.createClientResponse(messagesBytes...)
		if err != nil {
			p.log.Errorf("Error while creating client response for pull request: %v", err)
			return
		}
		p.replyToClient(clientResponse, conn)

	default:
		p.log.Info(packet.Flag)
		p.log.Info("Packet flag not recognised. Packet dropped")

	}
}

// RegisterNewClient generates a fresh authentication token and
// saves it together with client's public configuration data
// in the list of all registered clients. After the client is registered the function creates an inbox directory
// for the client's inbox, in which clients messages will be stored.
func (p *ProviderServer) registerNewClient(clientBytes []byte) ([]byte, error) {
	var clientConf config.ClientConfig
	err := proto.Unmarshal(clientBytes, &clientConf)
	if err != nil {
		return nil, err
	}
	clientID := fmt.Sprintf("%x", clientConf.PubKey)

	token, err := helpers.SHA256([]byte("TMP_Token" + clientID))
	if err != nil {
		return nil, err
	}
	record := ClientRecord{id: clientID,
		host:   clientConf.Host,
		port:   clientConf.Port,
		pubKey: clientConf.PubKey,
		token:  token,
	}
	p.assignedClients[clientID] = record

	path := fmt.Sprintf("./inboxes/%s", clientID)
	exists, err := helpers.DirExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := os.MkdirAll(path, 0775); err != nil {
			return nil, err
		}
	}

	return token, nil
}

// Function is responsible for handling the registration request from the client.
// it registers the client in the list of all registered clients and send
// an authentication token back to the client.
func (p *ProviderServer) handleAssignRequest(packet []byte) ([]byte, error) {
	p.log.Info("Received assign request from the client")

	token, err := p.registerNewClient(packet)
	if err != nil {
		return nil, err
	}

	return config.WrapWithFlag(flags.TokenFlag, token)
}

// Function is responsible for handling the pull request received from the client.
// It first authenticates the client, by checking if the received token is valid.
// If yes, the function triggers the function for checking client's inbox
// and sending buffered messages. Otherwise, an error is returned.
func (p *ProviderServer) handlePullRequest(rqsBytes []byte) ([][]byte, error) {
	var request config.PullRequest
	err := proto.Unmarshal(rqsBytes, &request)
	if err != nil {
		return nil, err
	}
	clientID := fmt.Sprintf("%x", request.ClientPublicKey)

	p.log.Infof("Processing pull request: %s %s", clientID, string(request.Token))
	if p.authenticateUser(request.ClientPublicKey, request.Token) {
		signal, messagesBytes, err := p.fetchMessages(clientID)
		if err != nil {
			return nil, err
		}
		switch signal {
		case "NI":
			p.log.Info("Inbox does not exist. Sending signal to client.")
		case "EI":
			p.log.Info("Inbox is empty. Sending info to the client.")
		case "SI":
			p.log.Info("All messages from the inbox successfully sent to the client.")
		}
		return messagesBytes, nil
	} else {
		p.log.Warn("Authentication went wrong")
		return nil, errors.New("authentication went wrong")
	}
}

// AuthenticateUser compares the authentication token received from the client with
// the one stored by the provider. If tokens are the same, it returns true
// and false otherwise.
func (p *ProviderServer) authenticateUser(clientKey, clientToken []byte) bool {
	clientID := fmt.Sprintf("%x", clientKey)
	if bytes.Equal(p.assignedClients[clientID].token, clientToken) &&
		bytes.Equal(p.assignedClients[clientID].pubKey, clientKey) {
		// && signature check on message to make sure client actually owns this ID
		return true
	}
	p.log.Warnf("Non matching token: %s, %s", p.assignedClients[clientID].token, clientToken)
	return false
}

// FetchMessages fetches messages from the requested inbox.
// FetchMessages checks whether an inbox exists and if it contains
// stored messages. If inbox contains any stored messages, all of them
// are send to the client one by one. FetchMessages returns a code
// signalling whether (NI) inbox does not exist, (EI) inbox is empty,
// (SI) messages were send to the client; and an error.
func (p *ProviderServer) fetchMessages(clientID string) (string, [][]byte, error) {

	path := fmt.Sprintf("./inboxes/%s", clientID)
	exist, err := helpers.DirExists(path)
	if err != nil {
		return "", nil, err
	}
	if !exist {
		return "NI", nil, nil
	}
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return "", nil, err
	}
	if len(files) == 0 {
		return "EI", nil, nil
	}

	messagesBytes := make([][]byte, len(files))
	for i, f := range files {
		fullPath := filepath.Join(path, f.Name())
		dat, err := ioutil.ReadFile(fullPath)
		if err != nil {
			return "", nil, err
		}

		p.log.Infof("Found stored message for %s", clientID)
		p.log.Infof("Messages data: %v", string(dat))
		msgBytes, err := config.WrapWithFlag(flags.CommFlag, dat)
		if err != nil {
			return "", nil, err
		}
		messagesBytes[i] = msgBytes

		if err := os.Remove(fullPath); err != nil {
			p.log.Errorf("Failed to remove %v: %v", f, err)
		}
		p.log.Infof("Removed %v", fullPath)
	}
	return "SI", messagesBytes, nil
}

// StoreMessage saves the given message in the inbox defined by the given id.
// If the inbox address does not exist or writing into the inbox was unsuccessful
// the function returns an error
func (p *ProviderServer) storeMessage(message []byte, inboxID string, messageID string) error {
	path := fmt.Sprintf("./inboxes/%s", inboxID)
	fileName := path + "/" + messageID + ".txt"

	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(message)
	if err != nil {
		return err
	}

	p.log.Infof("Stored message for %s", inboxID)
	p.log.Infof("Stored message content: %v", string(message))
	return nil
}

// NewProviderServer constructs a new provider object.
// NewProviderServer returns a new provider object and an error.
// TODO: same case as 'NewClient'
func NewProviderServer(id string,
	host string,
	port string,
	prvKey *sphinx.PrivateKey,
	pubKey *sphinx.PublicKey,
) (*ProviderServer, error) {
	baseLogger, err := logger.New(defaultLogFileLocation, defaultLogLevel, false)
	if err != nil {
		return nil, err
	}

	log := baseLogger.GetLogger(id)

	node := node.NewMix(prvKey, pubKey)
	providerServer := ProviderServer{id: id,
		host:     host,
		port:     port,
		Mix:      node,
		listener: nil,
		haltedCh: make(chan struct{}),
		log:      log,
	}
	providerServer.config = config.MixConfig{Id: providerServer.id,
		Host:   providerServer.host,
		Port:   providerServer.port,
		PubKey: providerServer.GetPublicKey().Bytes()}
	providerServer.assignedClients = make(map[string]ClientRecord)

	if err := helpers.RegisterMixProviderPresence(providerServer.GetPublicKey(),
		providerServer.convertRecordsToModelData(),
		net.JoinHostPort(host, port),
	); err != nil {
		return nil, err
	}

	// if err := helpers.RegisterMixProviderPresence(providerServer.host+providerServer.port, providerServer.GetPublicKey(), providerServer.convertRecordsToModelData()); err != nil {
	// 	return nil, err
	// }

	// addr, err := helpers.ResolveTCPAddress(providerServer.host, providerServer.port)
	// if err != nil {
	// 	return nil, err
	// }
	providerServer.listener, err = net.Listen("tcp", ":"+providerServer.port)

	if err != nil {
		return nil, err
	}

	return &providerServer, nil
}

func CreateTestProvider() (*ProviderServer, error) {
	priv, pub, err := sphinx.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	baseDisabledLogger, err := logger.New(defaultLogFileLocation, defaultLogLevel, true)
	if err != nil {
		return nil, err
	}
	// this logger can be shared as it will be disabled anyway
	disabledLog := baseDisabledLogger.GetLogger("test")

	node := node.NewMix(priv, pub)
	provider := ProviderServer{host: "localhost", port: "9999", Mix: node, log: disabledLog}
	provider.config = config.MixConfig{Id: provider.id,
		Host:   provider.host,
		Port:   provider.port,
		PubKey: provider.GetPublicKey().Bytes(),
	}
	provider.assignedClients = make(map[string]ClientRecord)
	return &provider, nil
}
