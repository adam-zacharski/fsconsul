package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

func main() {
	logrus.SetLevel(logrus.DebugLevel)
	os.Exit(realMain())
}

func realMain() int {
	var consulAddr string
	var consulDC string
	var keystore string
	var token string
	var configFile string
	var once bool

	// This will hold the configuration, whether it's resolved from command-line or JSON.
	var config WatchConfig

	flag.Usage = usage
	flag.StringVar(
		&consulAddr, "addr", "",
		"consul HTTP API address with port")
	flag.StringVar(
		&consulDC, "dc", "",
		"consul datacenter, uses local if blank")
	flag.StringVar(
		&keystore, "keystore", "",
		"directory of keys used for decryption")
	flag.StringVar(
		&token, "token", "",
		"token to use for ACL access")
	flag.BoolVar(
		&once, "once", false,
		"run once and exit")
	flag.StringVar(
		&configFile, "configFile", "",
		"json file containing all configuration (if this is provided, all other config is ignored)")
	flag.Parse()
	if configFile == "" && flag.NArg() < 2 {
		flag.Usage()
		return 1
	}

	// Setup the logging
	var log = logrus.New()
	log.Out = os.Stderr

	log.Info("fsconsul initializing...")

	args := flag.Args()

	if configFile != "" {
		// Load the configuration from JSON.
		configBody, err := ioutil.ReadFile(configFile)
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to read config file")
			return 2
		}

		err = json.Unmarshal(configBody, &config)
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to parse JSON")
			return 3
		}
	} else {
		// Build the configuraiton from the command-line
		var onChange []string
		if len(args) > 2 {
			onChange = args[2:]
		}

		// Check whether multiple paths / prefixes are specified
		var prefixes = strings.Split(args[0], "|")
		var paths = strings.Split(args[1], "|")

		if len(prefixes) != len(paths) {
			log.Error("There must be an identical number of prefixes and paths.")
			return 1
		}

		config = WatchConfig{
			RunOnce: once,
			Consul: ConsulConfig{
				Addr:  consulAddr,
				DC:    consulDC,
				Token: token,
			},
			Mappings: make([]MappingConfig, len(prefixes)),
		}

		for i := 0; i < len(prefixes); i++ {
			config.Mappings[i] = MappingConfig{
				Prefix:   prefixes[i],
				Path:     paths[i],
				Keystore: keystore,
				OnChange: onChange,
			}
		}
	}

	return watchAndExec(&config)
}

func usage() {
	cmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, strings.TrimSpace(helpText)+"\n\n", cmd)
	flag.PrintDefaults()
}

const helpText = `
Usage: %s [options] prefix path onchange

  Write files to the specified locations on the local system by reading K/Vs
  from Consul's K/V store with the given prefixes and executing a program on
  any change.  Prefixes and paths must be pipe-delimited if provided as
  command-line switches.

Options:
`
