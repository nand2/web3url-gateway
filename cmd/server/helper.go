package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"regexp"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/naoina/toml"
	log "github.com/sirupsen/logrus"

	"github.com/web3-protocol/web3protocol-go"
)

type Web3Config struct {
	ServerPort      string
	Verbosity       int
	CertificateFile string
	KeyFile         string
	RunAsHttp       bool
	AutoCertEmail   string
	SystemCertDir   string
	DefaultChain    int
	HomePage        string
	CORS            string
	PageCache       PageCacheConfig
	NSDefaultChains map[string]int
	Name2Chain      map[string]int
	ChainConfigs    map[int]ChainConfig
	RequestLimit    int
}

type PageCacheConfig struct {
	Enabled             bool
	MaxEntries          int
	MaxEntrySize        int // In bytes
	CacheDuration       int // In seconds
	ImmutableUrlRegexps []string
}

type NameServiceInfo struct {
	NSType web3protocol.DomainNameService
	NSAddr string
}

type ChainConfig struct {
	ChainID                  int
	RPC                      string
	RPCMaxConcurrentRequests int
	SystemRPC                string
	NSConfig                 map[string]NameServiceInfo
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return strings.Join(*i, ",")
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

type stringFlags struct {
	set   bool
	value string
}

func (sf *stringFlags) String() string {
	return sf.value
}

func (sf *stringFlags) Set(value string) error {
	sf.value = value
	sf.set = true
	return nil
}

// loadConfig loads the TOML config file from provided path if it exists
func loadConfig(file string, cfg *Web3Config) error {
	if file == "" {
		return fmt.Errorf("config file not specified")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}

	defer func(f *os.File) {
		err = f.Close()
	}(f)

	err = toml.NewDecoder(bufio.NewReader(f)).Decode(cfg)
	if _, ok := err.(*toml.LineError); ok {
		err = fmt.Errorf(file + ", " + err.Error())
	}
	return err
}

func getDefaultNSSuffix() (string, error) {
	if config.DefaultChain == 0 {
		return "", fmt.Errorf("default chain is not specified")
	}
	chainConfig, ok := config.ChainConfigs[config.DefaultChain]
	if !ok {
		return "", fmt.Errorf("cannot find chain config for default chain %v", config.DefaultChain)
	}
	// use first ns config as default
	for suffix := range chainConfig.NSConfig {
		return suffix, nil
	}
	return "", fmt.Errorf("cannot find ns config for default chain %v", config.DefaultChain)
}

func stats(returnSize int, hostPort string, targetChain string, nsType, path, host string) {
	point := influxdb2.NewPointWithMeasurement("w3stats").
		AddTag("chain", getChainById(targetChain)).
		AddTag("type", nsType).
		AddField("size", returnSize).
		SetTime(time.Now())
	er := writeAPI.WritePoint(context.Background(), point)
	if er != nil {
		log.Errorln("db err", er)
	}
	ip, _, er := net.SplitHostPort(hostPort)
	if er != nil {
		ip = "unknown"
	}
	point = influxdb2.NewPointWithMeasurement("w3stats_url").
		AddTag("url", host).
		AddField("ip", ip).
		SetTime(time.Now())
	er = writeAPI.WritePoint(context.Background(), point)
	if er != nil {
		log.Errorln("db err", er)
	}
	if path == "/" || path == "/index.html" {
		if net.ParseIP(host) == nil {
			point = influxdb2.NewPointWithMeasurement("w3stats_homepage").
				AddTag("url", host).
				AddField("ip", ip).
				SetTime(time.Now())
			er = writeAPI.WritePoint(context.Background(), point)
			if er != nil {
				log.Errorln("db err", er)
			}
		}
	}
}

func getChainById(chainId string) string {
	chainIdInt, err := strconv.Atoi(chainId)
	if err == nil {
		for k, v := range config.Name2Chain {
			if chainIdInt == v {
				return k
			}
		}
	}
	return chainId
}

// For a given hostname with a chain short name, replace by its chaid id. Examples:
// uniswap.eth:gor -> uniswap.eth:5
// uniswap.eth:5 -> uniswap.eth:5
// uniswap.eth -> uniswap.eth
func hostChangeChainShortNameToId(host string) string {
	hostParts := strings.Split(host, ":")
	if len(hostParts) == 1 {
		return hostParts[0]
	}

	var chainId string
	if _, ok := config.Name2Chain[hostParts[1]]; ok {
		chainId = fmt.Sprintf("%d", config.Name2Chain[hostParts[1]])
	} else {
		chainId = hostParts[1]
	}

	return hostParts[0] + ":" + chainId
}

// Converts a web3:// URL to a Gateway URL
func ConvertWeb3UrlToGatewayUrl(web3Url string, rootGatewayHost string) (string, error) {
	// Parse the URL
	re := regexp.MustCompile(`^(?P<protocol>[^:]+):\/\/(?P<hostname>[^:\/?#]+)(:(?P<chainId>[1-9][0-9]*))?(?P<pathQuery>(?P<path>\/[^?#]*)?([?](?P<query>[^#]*))?)?(#(?P<fragment>.*)?)?$`)
	match := re.FindStringSubmatch(web3Url)
	if match == nil {
		// Invalid web3:// URL
		return "", fmt.Errorf("invalid web3 URL: %s", web3Url)
	}

	// Extract named groups
	urlMainParts := make(map[string]string)
	for i, name := range re.SubexpNames() {
		if i != 0 && name != "" {
			urlMainParts[name] = match[i]
		}
	}

	// Check protocol name
	webProtocol := urlMainParts["protocol"]
	if webProtocol != "web3" && webProtocol != "w3" {
		// Bad protocol name
		return "", fmt.Errorf("invalid web3 URL: %s", web3Url)
	}

	var subDomains []string

	// Is the contract an ethereum address?
	isEthAddress, _ := regexp.MatchString(`^0x[0-9a-fA-F]{40}$`, urlMainParts["hostname"])
	if isEthAddress {
		subDomains = append(subDomains, urlMainParts["hostname"])
		if urlMainParts["chainId"] != "" {
			subDomains = append(subDomains, urlMainParts["chainId"])
		} else {
			subDomains = append(subDomains, "1")
		}
	} else {
		// It is a domain name
		if strings.HasSuffix(urlMainParts["hostname"], ".eth") && urlMainParts["chainId"] == "" {
			subDomains = append(subDomains, urlMainParts["hostname"])
			subDomains = append(subDomains, "1")
		} else {
			subDomains = append(subDomains, urlMainParts["hostname"])
			if urlMainParts["chainId"] != "" {
				subDomains = append(subDomains, urlMainParts["chainId"])
			}
		}
	}

	path := urlMainParts["path"]
	if path == "" {
		path = "/"
	}

	protocol := "https"
	if config.RunAsHttp {
		protocol = "http"
	}

	gatewayUrl := protocol + "://" + strings.Join(subDomains, ".") + "." + rootGatewayHost + path
	return gatewayUrl, nil
}


func requestLimiter(next http.Handler) http.Handler {
	var semaphore = make(chan struct{}, config.RequestLimit)
	log.Infof("Request limit: %v\n", config.RequestLimit)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case semaphore <- struct{}{}: // Acquire a slot
			defer func() { <-semaphore }() // Release the slot when done
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "Too many requests, please try again later.", http.StatusServiceUnavailable)
		}
	})
}
