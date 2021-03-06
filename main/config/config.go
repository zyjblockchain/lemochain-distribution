package config

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"github.com/LemoFoundationLtd/lemochain-core/chain/deputynode"
	"github.com/LemoFoundationLtd/lemochain-core/common"
	"github.com/LemoFoundationLtd/lemochain-core/common/crypto"
	"github.com/LemoFoundationLtd/lemochain-core/common/hexutil"
	"github.com/LemoFoundationLtd/lemochain-core/common/log"
	"github.com/LemoFoundationLtd/lemochain-core/network/p2p"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	ConfigGuideUrl    = ""
	datadirPrivateKey = "nodekey"
	configName        = "distribution-config.json"
)

const (
	DefaultHttpAddr         = "127.0.0.1"
	DefaultHttpPort         = 8001
	DefaultHttpVirtualHosts = "localhost"
	DefaultWSAddr           = "127.0.0.1"
	DefaultWSPort           = 8002
)

//go:generate gencodec -type RpcHttp -field-override RpcMarshaling -out gen_http_json.go
//go:generate gencodec -type RpcWS -field-override RpcMarshaling -out gen_ws_json.go
//go:generate gencodec -type Config -field-override ConfigMarshaling -out gen_config_json.go

type RpcHttp struct {
	Disable       bool   `json:"disable"`
	Port          uint32 `json:"port"  gencodec:"required"`
	CorsDomain    string `json:"corsDomain"`
	VirtualHosts  string `json:"virtualHosts"`
	ListenAddress string `json:"listenAddress"`
}

type RpcMarshaling struct {
	Port hexutil.Uint32
}

type RpcWS struct {
	Disable       bool   `json:"disable"`
	Port          uint32 `json:"port"`
	CorsDomain    string `json:"corsDomain"`
	ListenAddress string `json:"listenAddress"`
}

type Config struct {
	ChainID     uint32  `json:"chainID"        gencodec:"required"`
	DeputyCount uint32  `json:"deputyCount"    gencodec:"required"`
	DataDir     string  `json:"serverDataDir"  gencodec:"required"`
	DbUri       string  `json:"dbUri"          gencodec:"required"` // sample: root:123123@tcp(localhost:3306)/lemochain?charset=utf8mb4
	DbDriver    string  `json:"dbDriver"       gencodec:"required"`
	LogLevel    uint32  `json:"logLevel"       gencodec:"required"`
	CoreNode    string  `json:"coreNode"       gencodec:"required"`
	Http        RpcHttp `json:"http"`
	WebSocket   RpcWS   `json:"webSocket"`

	nodeKey      *ecdsa.PrivateKey
	coreNodeID   *p2p.NodeID
	coreEndpoint string
}

type ConfigMarshaling struct {
	ChainID     hexutil.Uint32
	DeputyCount hexutil.Uint32
	LogLevel    hexutil.Uint32
}

func ReadConfigFile() (*Config, error) {
	// Try to read from system temp directory
	dir := filepath.Dir(os.Args[0])
	filePath := filepath.Join(dir, configName)
	if _, err := os.Stat(filePath); err != nil {
		// Try to read from relative path
		filePath = configName
	}
	log.Infof("Load config file: %s", filePath)
	file, err := os.Open(filePath)
	if err != nil {
		return nil, errors.New(err.Error() + "\r\n" + ConfigGuideUrl)
	}

	var config Config
	if err = json.NewDecoder(file).Decode(&config); err != nil {
		return nil, ErrConfig
	}
	deputynode.SetSelfNodeKey(config.NodeKey())
	if err := check(&config); err != nil {
		return nil, err
	}
	return &config, nil
}

func check(cfg *Config) error {
	if cfg.ChainID == 0 {
		return ErrChainId
	}
	if cfg.LogLevel > 5 {
		return ErrLogLevel
	}
	if !cfg.Http.Disable {
		if cfg.Http.ListenAddress == "" {
			cfg.Http.ListenAddress = DefaultHttpAddr
		}

		if cfg.Http.Port > 65535 {
			return ErrPort
		} else if cfg.Http.Port == 0 {
			cfg.Http.Port = DefaultHttpPort
		}

		if cfg.Http.VirtualHosts == "" {
			cfg.Http.VirtualHosts = DefaultHttpVirtualHosts
		}
	}
	if !cfg.WebSocket.Disable {
		if cfg.WebSocket.ListenAddress == "" {
			cfg.WebSocket.ListenAddress = DefaultWSAddr
		}

		if cfg.WebSocket.Port > 65535 {
			return ErrPort
		} else if cfg.WebSocket.Port == 0 {
			cfg.WebSocket.Port = DefaultWSPort
		}
	}
	nodeID, endpoint := checkNodeString(cfg.CoreNode)
	if nodeID == nil {
		return ErrCoreNode
	}
	cfg.coreNodeID = nodeID
	cfg.coreEndpoint = endpoint
	return nil
}

func (c *Config) NodeKey() *ecdsa.PrivateKey {
	if c.nodeKey != nil {
		return c.nodeKey
	}

	keyFile := filepath.Join(c.DataDir, datadirPrivateKey)
	if key, err := crypto.LoadECDSA(keyFile); err == nil {
		c.nodeKey = key
		return key
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		log.Critf("Failed to generate node key: %v", err)
	}
	instanceDir, _ := filepath.Abs(c.DataDir)
	if err := os.MkdirAll(instanceDir, 0700); err != nil {
		log.Errorf("Failed to persist node key: %v", err)
		return key
	}
	keyFile = filepath.Join(instanceDir, datadirPrivateKey)
	if err := crypto.SaveECDSA(keyFile, key); err != nil {
		log.Errorf("Failed to persist node key: %v", err)
	}
	c.nodeKey = key
	return key
}

func (c *Config) CoreNodeID() *p2p.NodeID {
	return c.coreNodeID
}

func (c *Config) CoreEndpoint() string {
	return c.coreEndpoint
}

// checkNodeString verify invalid
func checkNodeString(node string) (*p2p.NodeID, string) {
	tmp := strings.Split(node, "@")
	if len(tmp) != 2 {
		return nil, ""
	}
	var nodeID = tmp[0]
	var IP = tmp[1]

	// nodeID
	if len(nodeID) != 128 {
		return nil, ""
	}
	parsedNodeID := p2p.BytesToNodeID(common.FromHex(nodeID))
	_, err := parsedNodeID.PubKey()
	if err != nil {
		return nil, ""
	}
	if bytes.Compare(parsedNodeID[:], deputynode.GetSelfNodeID()) == 0 {
		return nil, ""
	}

	// IP
	if !verifyIP(IP) {
		return nil, ""
	}
	return parsedNodeID, IP
}

// verifyIP  verify ipv4
func verifyIP(input string) bool {
	tmp := strings.Split(input, ":")
	if len(tmp) != 2 {
		return false
	}
	if ip := net.ParseIP(tmp[0]); ip == nil {
		return false
	}
	p, err := strconv.Atoi(tmp[1])
	if err != nil {
		return false
	}
	if p < 1024 || p > 65535 {
		return false
	}
	return true
}
