package group

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
)

type ICEServer struct {
	URLs           []string    `json:"urls"`
	Username       string      `json:"username,omitempty"`
	Credential     interface{} `json:"credential,omitempty"`
	CredentialType string      `json:"credentialType,omitempty"`
}

type RTCConfiguration struct {
	ICEServers         []ICEServer `json:"iceServers,omitempty"`
	ICETransportPolicy string      `json:"iceTransportPolicy,omitempty"`
}

// draft-uberti-behave-turn-rest-00 Section 2.2
type RTCHTTPConfiguration struct {
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	TTL      int64    `json:"ttl,omitempty"`
	URIs     []string `json:"uris,omitempty"`
}

var ICEFilename string
var ICEURL string
var ICERelayOnly bool

type iceConf struct {
	httpConf      RTCHTTPConfiguration
	httpTimestamp time.Time
	conf          RTCConfiguration
	timestamp     time.Time
}

var iceConfiguration atomic.Value

func getHTTPConf() (conf RTCHTTPConfiguration, err error) {
	client := http.Client{
		Timeout: 30 * time.Second,
	}
	var resp *http.Response
	resp, err = client.Get(ICEURL)
	if err != nil {
		return
	}
	if resp.StatusCode != 200 {
		err = errors.New(resp.Status)
		return
	}
	d := json.NewDecoder(resp.Body)
	err = d.Decode(&conf)
	if err != nil {
		return
	}

	return

}

func updateICEConfiguration() *iceConf {
	now := time.Now()
	var conf RTCConfiguration

	if ICEURL != "" {
		var deadline time.Time
		var httpConf RTCHTTPConfiguration
		old, ok := iceConfiguration.Load().(*iceConf)
		if ok {
			deadline = old.httpTimestamp.Add(
				time.Duration(old.httpConf.TTL) * time.Second,
			)
			httpConf = old.httpConf
		}

		if now.Add(-5 * time.Minute).After(deadline) {
			c, err := getHTTPConf()
			if err != nil {
				log.Printf(
					"Get ICE configuration over HTTP: %v",
					err,
				)
			} else {
				httpConf = c
			}
		}

		if len(httpConf.URIs) > 0 {
			conf.ICEServers = append(conf.ICEServers,
				ICEServer {
					URLs: httpConf.URIs,
					Username: httpConf.Username,
					Credential: httpConf.Password,
				})
		}
	}

	if ICEFilename != "" {
		file, err := os.Open(ICEFilename)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("Open %v: %v", ICEFilename, err)
			}
		} else {
			d := json.NewDecoder(file)
			var servers []ICEServer
			err = d.Decode(&servers)
			file.Close()
			if err != nil {
				log.Printf("Get ICE configuration: %v", err)
			} else {
				conf.ICEServers =
					append(conf.ICEServers, servers...)
			}
		}
	}

	if ICERelayOnly {
		conf.ICETransportPolicy = "relay"
	}

	iceConf := iceConf{
		conf:      conf,
		timestamp: now,
	}
	iceConfiguration.Store(&iceConf)
	return &iceConf
}

func ICEConfiguration() *RTCConfiguration {
	conf, ok := iceConfiguration.Load().(*iceConf)
	if !ok || time.Since(conf.timestamp) > 5*time.Minute {
		conf = updateICEConfiguration()
	} else if time.Since(conf.timestamp) > 2*time.Minute {
		go updateICEConfiguration()
	}

	return &conf.conf
}

func ToConfiguration(conf *RTCConfiguration) webrtc.Configuration {
	var iceServers []webrtc.ICEServer
	for _, s := range conf.ICEServers {
		tpe := webrtc.ICECredentialTypePassword
		if s.CredentialType == "oauth" {
			tpe = webrtc.ICECredentialTypeOauth
		}
		iceServers = append(iceServers,
			webrtc.ICEServer{
				URLs:           s.URLs,
				Username:       s.Username,
				Credential:     s.Credential,
				CredentialType: tpe,
			},
		)
	}

	policy := webrtc.ICETransportPolicyAll
	if conf.ICETransportPolicy == "relay" {
		policy = webrtc.ICETransportPolicyRelay
	}

	return webrtc.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: policy,
	}
}
