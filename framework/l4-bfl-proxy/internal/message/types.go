package message

import (
	"github.com/beclab/l4-bfl-proxy/internal/ir"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/telepresenceio/watchable"
)

type UserInfo struct {
	Name              string
	Namespace         string
	Did               string
	Zone              string
	IsEphemeral       bool
	BFLHost           string
	BFLPort           int
	AccessLevel       uint64
	AllowCIDRs        []string
	DenyAll           bool
	AllowedDomains    []string
	ServerNameDomains []string
	LocalDomainIP     string
	CreateTimestamp   int64
}

type AppInfo struct {
	Name      string
	Appid     string
	Owner     string
	Entrances []EntranceInfo
	Ports     []PortInfo
}

type EntranceInfo struct {
	Name      string
	AuthLevel string
}

type PortInfo struct {
	Name       string
	Host       string
	Port       int32
	ExposePort int32
	Protocol   string
}

type Resources struct {
	Users []*UserInfo
	Apps  []*AppInfo
}

type ProviderResources struct {
	watchable.Map[string, *Resources]
}

type XdsIR struct {
	watchable.Map[string, *ir.Xds]
}

type XdsSnapshot struct {
	Listeners []cachetypes.Resource
	Clusters  []cachetypes.Resource
}

type XdsResources struct {
	watchable.Map[string, *XdsSnapshot]
}

func (p *ProviderResources) Close() {
	p.Map.Close()
}

func (x *XdsIR) Close() {
	x.Map.Close()
}

func (x *XdsResources) Close() {
	x.Map.Close()
}
