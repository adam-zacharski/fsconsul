package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"text/template"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/armed/mkdirp"
	consulapi "github.com/hashicorp/consul/api"

	gosecret "github.com/cimpress-mcp/gosecret/api"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

// ConsulConfig holds the configuration for Consul
type ConsulConfig struct {
	Addr  string
	DC    string
	Token string

	KeyFile  string
	CertFile string
	CAFile   string
	UseTLS   bool
}

// MappingConfig holds configuration for all mappings from KV to fs managed by this process.
type MappingConfig struct {
	OnChange    []string
	OnChangeRaw string `json:"onchange"`
	Prefix      string
	Path        string
	Keystore    string
}

// WatchConfig holds fsconsul configuration
type WatchConfig struct {
	RunOnce  bool
	Consul   ConsulConfig
	Mappings []MappingConfig
}

func applyDefaults(config *WatchConfig) {
	if config.Consul.Addr == "" {
		config.Consul.Addr = "127.0.0.1:8500"
	}
}

// Queue watchers
func watchAndExec(config *WatchConfig) int {

	applyDefaults(config)

	returnCodes := make(chan int)

	// Fork a separate goroutine for each prefix/path pair
	for i := 0; i < len(config.Mappings); i++ {
		go func(mappingConfig *MappingConfig) {

			if mappingConfig.OnChangeRaw != "" {
				mappingConfig.OnChange = strings.Split(mappingConfig.OnChangeRaw, " ")
			}

			log.WithFields(log.Fields{
				"config": mappingConfig,
			}).Debug("Got mapping config")

			returnCode, err := watchMappingAndExec(config, mappingConfig)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
				}).Debug("Failure from watch function")
			}

			returnCodes <- returnCode
		}(&config.Mappings[i])
	}

	// Wait for completion of all forked go routines
	failures := false
	for i := 0; i < len(config.Mappings); i++ {
		returnCode := <-returnCodes
		log.Debug(returnCode)
		if returnCode != 0 {
			failures = true
		}
	}

	if failures {
		return -1
	}

	return 0
}

func buildClient(consulConfig ConsulConfig) (*http.Client, error) {
	tlsConfig := &tls.Config{}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	client := &http.Client{Transport: transport}

	// Check if the user defined a specific CA to use
	if consulConfig.CAFile != "" {
		certPool := x509.NewCertPool()
		if data, err := ioutil.ReadFile(consulConfig.CAFile); err != nil {
			return nil, err
		} else if !certPool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("Invalid certificate file: %s", consulConfig.CAFile)
		}
		tlsConfig.RootCAs = certPool
	}

	// Check if TLS was configured for client-side verification
	if consulConfig.CertFile != "" && consulConfig.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(consulConfig.CertFile, consulConfig.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return client, nil
}

func buildConsulClient(consulConfig ConsulConfig) (client *consulapi.Client, err error) {
	kvConfig := consulapi.DefaultConfig()
	kvConfig.Address = consulConfig.Addr
	kvConfig.Datacenter = consulConfig.DC

	// Enforce use of secure connection
	if consulConfig.UseTLS {
		kvConfig.Scheme = "https"
	}

	kvConfig.HttpClient, err = buildClient(consulConfig)
	if err != nil {
		return nil, err
	}

	client, err = consulapi.NewClient(kvConfig)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Connects to Consul and watches a given K/V prefix and uses that to
// write to the filesystem.
func watchMappingAndExec(config *WatchConfig, mappingConfig *MappingConfig) (int, error) {
	client, err := buildConsulClient(config.Consul)
	if err != nil {
		return 0, err
	}

	// If prefix starts with /, trim it.
	if mappingConfig.Prefix[0] == '/' {
		mappingConfig.Prefix = mappingConfig.Prefix[1:]
	}

	// If the config path is lacking a trailing separator, add it.
	if mappingConfig.Path[len(mappingConfig.Path)-1] != os.PathSeparator {
		mappingConfig.Path += string(os.PathSeparator)
	}

	isWindows := os.PathSeparator != '/'

	// Remove an unhandled trailing quote, which presented itself on Windows when
	// the given path contained spaces (requiring quotes) and also had a trailing
	// backslash.
	if mappingConfig.Path[len(mappingConfig.Path)-1] == 34 {
		mappingConfig.Path = mappingConfig.Path[:len(mappingConfig.Path)-1]
	}

	// Start the watcher goroutine that watches for changes in the
	// K/V and notifies us on a channel.
	errCh := make(chan error, 1)
	pairCh := make(chan consulapi.KVPairs)
	quitCh := make(chan struct{})

	// Defer close of quitCh if we're running more than once
	if !config.RunOnce {
		defer close(quitCh)
	}

	go watch(
		client, mappingConfig.Prefix, mappingConfig.Path, config.Consul.Token, pairCh, errCh, quitCh)

	var env map[string]string
	for {
		var pairs consulapi.KVPairs

		// Wait for new pairs to come on our channel or an error
		// to occur.
		select {
		case pairs = <-pairCh:
		case err := <-errCh:
			return 0, err
		}

		newEnv := make(map[string]string)
		for _, pair := range pairs {
			log.WithFields(log.Fields{
				"key": pair.Key,
			}).Debug("Key present in source")
			k := strings.TrimPrefix(pair.Key, mappingConfig.Prefix)
			k = strings.TrimLeft(k, "/")
			newEnv[k] = string(pair.Value)
		}

		// If the variables didn't actually change,
		// then don't do anything.
		if reflect.DeepEqual(env, newEnv) {
			continue
		}

		// Iterate over all objects in the current env.  If they are not in the newEnv, they
		// were deleted from Consul and should be deleted from disk.
		for k := range env {
			if _, ok := newEnv[k]; !ok {
				log.WithFields(log.Fields{
					"key": k,
				}).Debug("Key no longer present locally")
				// Write file to disk
				keyfile := fmt.Sprintf("%s%s", mappingConfig.Path, k)
				if isWindows {
					keyfile = strings.Replace(keyfile, "/", "\\", -1)
				}

				err := os.Remove(keyfile)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Failed to remove key")
				}
			}
		}

		// Replace the env so we can detect future changes
		env = newEnv

		// Write the updated keys to the filesystem at the specified path
		for k, v := range newEnv {
			// Write file to disk
			keyfile := fmt.Sprintf("%s%s", mappingConfig.Path, k)

			// if Windows, replace / with windows path delimiter
			if isWindows {
				keyfile = strings.Replace(keyfile, "/", "\\", -1)
				// mkdirp the file's path
				err := mkdirp.Mk(keyfile[:strings.LastIndex(keyfile, "\\")], 0777)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Failed to create parent directory for key")
				}
			} else {
				// mkdirp the file's path
				err := mkdirp.Mk(keyfile[:strings.LastIndex(keyfile, "/")], 0777)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Failed to create parent directory for key")
				}
			}

			f, err := os.Create(keyfile)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"file":  keyfile,
				}).Error("Failed to create file")
				continue
			}

			defer f.Close()

			log.WithFields(log.Fields{
				"length": len(v),
			}).Debug("Input value length")

			buff := new(bytes.Buffer)
			buff.Write([]byte(v))

			if len(mappingConfig.Keystore) > 0 {
				decryptedValue, err := gosecret.DecryptTags([]byte(v), mappingConfig.Keystore)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Failed to decrypt value")
					continue
				}

				log.WithFields(log.Fields{
					"length": len(decryptedValue),
				}).Debug("Output value length")

				data := string(decryptedValue)

				funcs := template.FuncMap{
					// Template functions
					"goDecrypt": goDecryptFunc(mappingConfig.Keystore),
				}

				tmpl, err := template.New("decryption").Funcs(funcs).Parse(data)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Could not parse template")
					continue
				}

				// Run the template to verify the output.
				buff = new(bytes.Buffer)
				err = tmpl.Execute(buff, nil)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Could not execute template")
					continue
				}
			}

			wrote, err := f.Write(buff.Bytes())
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"file":  keyfile,
				}).Error("Failed to write to file")
				continue
			}

			log.WithFields(log.Fields{
				"length": wrote,
				"file":   keyfile,
			}).Debug("Successfully wrote value to file")

			err = f.Sync()
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"file":  keyfile,
				}).Error("Failed to sync file")
			}

			err = f.Close()
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"file":  keyfile,
				}).Error("Failed to close file")
			}
		}

		// Configuration changed, run our onchange command, if one was specified.
		if mappingConfig.OnChange != nil {
			var cmd = exec.Command(mappingConfig.OnChange[0], mappingConfig.OnChange[1:]...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			// Always wait for the forked process to exit.  We may wish to revisit this, but I think
			// it's the safest approach since it avoids a case where rapid key updates DOS a system
			// by slurping all proc handles.
			err = cmd.Run()

			if err != nil {
				return 111, err
			}
		}

		// If we are only running once, close the channel on this watcher.
		if config.RunOnce {
			close(quitCh)
			return 0, nil
		}
	}
}

func watch(
	client *consulapi.Client,
	prefix string,
	path string,
	token string,
	pairCh chan<- consulapi.KVPairs,
	errCh chan<- error,
	quitCh <-chan struct{}) {

	// Create the root for KVs, if necessary
	mkdirp.Mk(path, 0777)

	// Get the initial list of k/v pairs. We don't do a retryableList
	// here because we want a fast fail if the initial request fails.
	opts := &consulapi.QueryOptions{Token: token}
	pairs, meta, err := client.KV().List(prefix, opts)
	if err != nil {
		errCh <- err
		return
	}

	// Send the initial list out right away
	pairCh <- pairs

	// Loop forever (or until quitCh is closed) and watch the keys
	// for changes.
	curIndex := meta.LastIndex
	for {
		select {
		case <-quitCh:
			return
		default:
		}

		pairs, meta, err = retryableList(
			func() (consulapi.KVPairs, *consulapi.QueryMeta, error) {
				opts = &consulapi.QueryOptions{WaitIndex: curIndex, Token: token}
				return client.KV().List(prefix, opts)
			})

		if err != nil {
			// This happens when the connection to the consul agent dies.  Build in a retry by looping after a delay.
			log.Warn("Error communicating with consul agent.")
			continue
		}

		pairCh <- pairs
		log.WithFields(log.Fields{
			"curIndex":  curIndex,
			"lastIndex": meta.LastIndex,
		}).Debug("Potential index update observed")
		curIndex = meta.LastIndex
	}
}

// This function is able to call KV listing functions and retry them.
// We want to retry if there are errors because it is safe (GET request),
// and erroring early is MUCH more costly than retrying over time and
// delaying the configuration propagation.
func retryableList(f func() (consulapi.KVPairs, *consulapi.QueryMeta, error)) (consulapi.KVPairs, *consulapi.QueryMeta, error) {
	i := 0
	for {
		p, m, e := f()
		if e != nil {
			if i >= 3 {
				return nil, nil, e
			}

			i++

			// Reasonably arbitrary sleep to just try again... It is
			// a GET request so this is safe.
			time.Sleep(time.Duration(i*2) * time.Second)
		}

		return p, m, e
	}
}
