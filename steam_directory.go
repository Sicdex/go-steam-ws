package steam

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/sicdex/go-steam-ws/netutil"
)

// Load initial server list from Steam Directory Web API.
// Call InitializeSteamDirectory() before Connect() to use
// steam directory server list instead of static one.
func InitializeSteamDirectory() error {
	return steamDirectoryCache.Initialize()
}

var steamDirectoryCache *steamDirectory = &steamDirectory{}

type steamDirectory struct {
	sync.RWMutex
	servers       []string
	isInitialized bool
}

// Get server list from steam directory and save it for later
func (sd *steamDirectory) Initialize() error {
	sd.Lock()
	defer sd.Unlock()
	client := new(http.Client)
	resp, err := client.Get(fmt.Sprintf("https://api.steampowered.com/ISteamDirectory/GetCMList/v1/?cellId=0"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r := struct {
		Response struct {
			ServerList []string
			Result     uint32
			Message    string
		}
	}{}
	if err = json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Response.Result != 1 {
		return fmt.Errorf("Failed to get steam directory, result: %v, message: %v\n", r.Response.Result, r.Response.Message)
	}
	if len(r.Response.ServerList) == 0 {
		return fmt.Errorf("Steam returned zero servers for steam directory request\n")
	}
	sd.servers = r.Response.ServerList
	sd.isInitialized = true
	return nil
}

func (sd *steamDirectory) GetRandomCM() *netutil.PortAddr {
	sd.RLock()
	defer sd.RUnlock()
	if !sd.isInitialized {
		panic("steam directory is not initialized")
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	addr := netutil.ParsePortAddr(sd.servers[rng.Int31n(int32(len(sd.servers)))])
	return addr
}

func (sd *steamDirectory) IsInitialized() bool {
	sd.RLock()
	defer sd.RUnlock()
	isInitialized := sd.isInitialized
	return isInitialized
}

type CMServer struct {
	Endpoint       string
	LegacyEndpoint string
	Type           string
	DC             string
	Load           int
	WeightedLoad   float64
}

func FetchCMListForConnect(cellID uint32) ([]CMServer, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	url := fmt.Sprintf("https://api.steampowered.com/ISteamDirectory/GetCMListForConnect/v1/?cellid=%d", cellID)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Response struct {
			Serverlist []struct {
				Endpoint       string  `json:"endpoint"`
				LegacyEndpoint string  `json:"legacy_endpoint"`
				Type           string  `json:"type"`
				DC             string  `json:"dc"`
				Realm          string  `json:"realm"`
				Load           int     `json:"load"`
				WtdLoad        float64 `json:"wtd_load"`
			} `json:"serverlist"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode GetCMListForConnect: %w", err)
	}
	if len(r.Response.Serverlist) == 0 {
		return nil, fmt.Errorf("GetCMListForConnect returned zero servers")
	}
	out := make([]CMServer, 0, len(r.Response.Serverlist))
	for _, s := range r.Response.Serverlist {
		out = append(out, CMServer{
			Endpoint:       s.Endpoint,
			LegacyEndpoint: s.LegacyEndpoint,
			Type:           s.Type,
			DC:             s.DC,
			Load:           s.Load,
			WeightedLoad:   s.WtdLoad,
		})
	}
	return out, nil
}

func FilterByType(servers []CMServer, t string) []CMServer {
	out := servers[:0:0]
	for _, s := range servers {
		if s.Type == t {
			out = append(out, s)
		}
	}
	return out
}

func PickRandom(servers []CMServer) CMServer {
	if len(servers) == 0 {
		return CMServer{}
	}
	return servers[rand.Intn(len(servers))]
}
