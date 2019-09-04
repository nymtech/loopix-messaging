package main

import "github.com/tav/golly/optparse"

func main() {
	var logo = `
  _                      _      
 | |    ___   ___  _ __ (_)_  __
 | |   / _ \ / _ \| '_ \| \ \/ /
 | |___ (_) | (_) | |_) | |>  < 
 |_____\___/ \___/| .__/|_/_/\_\
		  |_|            (provider)
		  
		  `
	cmds := map[string]func([]string, string){
		"run": cmdRun,
	}
	info := map[string]string{
		"run": "Run a Loopix provider for offline storage",
	}
	optparse.Commands("loopix-provider", "0.0.1", cmds, info, logo)
}