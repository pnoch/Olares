package provider

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	iamv1alpha2 "github.com/beclab/api/iam/v1alpha2"
	"github.com/beclab/l4-bfl-proxy/internal/message"
	appv2alpha1 "github.com/beclab/l4-bfl-proxy/util/app/v2alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	mapKey          = "default"
	resyncPeriod    = 30 * time.Second
	dnsLookupRetry  = 15
	dnsRetryBackoff = 3 * time.Second
)

var (
	annotationGroup = "bytetrade.io"

	userAnnotationDid         = annotationGroup + "/did"
	userAnnotationZone        = annotationGroup + "/zone"
	userAnnotationOwnerRole   = annotationGroup + "/owner-role"
	userLauncherAccessLevel   = annotationGroup + "/launcher-access-level"
	userLauncherAllowCIDR     = annotationGroup + "/launcher-allow-cidr"
	userAnnotationCreator     = annotationGroup + "/creator"
	userAnnotationIsEphemeral = annotationGroup + "/is-ephemeral"
	userDenyAllPolicy         = annotationGroup + "/deny-all"
	userLocalDomainIPDns      = annotationGroup + "/local-domain-dns-record"

	settingsCustomDomain                 = "customDomain"
	settingsCustomDomainThirdLevelDomain = "third_level_domain"
	settingsCustomDomainThirdPartyDomain = "third_party_domain"

	applicationAuthLevelPublic = "public"

	iamUserGVR = schema.GroupVersionResource{
		Group:    "iam.kubesphere.io",
		Version:  "v1alpha2",
		Resource: "users",
	}
	appGVR = schema.GroupVersionResource{
		Group:    "app.bytetrade.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}
)

type Config struct {
	UserNamespacePrefix string
	BFLServicePort      int
	SSLServerPort       int
	SSLProxyServerPort  int
}

type Provider struct {
	client    dynamic.Interface
	resources *message.ProviderResources
	cfg       *Config
	userStore cache.Store
	appStore  cache.Store
	synced    atomic.Bool
}

func New(client dynamic.Interface, resources *message.ProviderResources, cfg *Config) *Provider {
	return &Provider{
		client:    client,
		resources: resources,
		cfg:       cfg,
	}
}

func (p *Provider) Name() string { return "provider" }

func (p *Provider) Start(ctx context.Context) error {
	klog.Info("provider: starting dynamic informers...")

	factory := dynamicinformer.NewDynamicSharedInformerFactory(p.client, resyncPeriod)
	userInformer := factory.ForResource(iamUserGVR).Informer()
	appInformer := factory.ForResource(appGVR).Informer()

	p.userStore = userInformer.GetStore()
	p.appStore = appInformer.GetStore()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) {
			if p.synced.Load() {
				p.publishResources()
			}
		},
		UpdateFunc: func(_, _ interface{}) {
			if p.synced.Load() {
				p.publishResources()
			}
		},
		DeleteFunc: func(_ interface{}) {
			if p.synced.Load() {
				p.publishResources()
			}
		},
	}
	if _, err := userInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("add user event handler: %w", err)
	}
	if _, err := appInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("add app event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	p.synced.Store(true)
	klog.Info("provider: informer caches synced, publishing initial snapshot")
	p.publishResources()

	<-ctx.Done()
	klog.Info("provider: stopped")
	return nil
}

func (p *Provider) publishResources() {
	users, err := p.listUsers()
	if err != nil {
		klog.Errorf("provider: list users: %v", err)
		return
	}

	apps, err := p.listApps()
	if err != nil {
		klog.Errorf("provider: list apps: %v", err)
		return
	}

	snapshot := &message.Resources{
		Users: users,
		Apps:  apps,
	}
	snapshot.Sort()

	if old, ok := p.resources.Load(mapKey); ok && old.Equal(snapshot) {
		klog.V(4).Info("provider: snapshot unchanged, skipping publish")
		return
	}

	p.resources.Store(mapKey, snapshot)
	klog.Infof("provider: published snapshot with %d users and %d apps", len(users), len(apps))
}

// listApps reads applications from the informer cache and builds AppInfo slice.
func (p *Provider) listApps() ([]*message.AppInfo, error) {
	appList := p.getAppsFromCache()

	var result []*message.AppInfo
	for _, app := range appList {
		entrances := make([]message.EntranceInfo, 0, len(app.Spec.Entrances))
		for _, e := range app.Spec.Entrances {
			entrances = append(entrances, message.EntranceInfo{
				Name:      e.Name,
				AuthLevel: e.AuthLevel,
			})
		}

		ports := make([]message.PortInfo, 0, len(app.Spec.Ports))
		for _, sp := range app.Spec.Ports {
			ports = append(ports, message.PortInfo{
				Name:       sp.Name,
				Host:       sp.Host,
				Port:       sp.Port,
				ExposePort: sp.ExposePort,
				Protocol:   sp.Protocol,
			})
		}

		result = append(result, &message.AppInfo{
			Name:      app.Spec.Name,
			Appid:     app.Spec.Appid,
			Owner:     app.Spec.Owner,
			Entrances: entrances,
			Ports:     ports,
		})
	}
	return result, nil
}

// listUsers reads users from the informer cache, parses annotations, resolves BFL host, builds UserInfo slice.
func (p *Provider) listUsers() ([]*message.UserInfo, error) {
	publicAppIDs, publicCustomDomainApps, _, customDomainAppsWithUsers := p.listApplicationDetails()

	userList := p.getUsersFromCache()

	getUserByName := func(name string) *iamv1alpha2.User {
		for i := range userList {
			if userList[i].Name == name {
				return &userList[i]
			}
			if name == "cli" && userList[i].Annotations[userAnnotationOwnerRole] == "owner" {
				return &userList[i]
			}
		}
		return nil
	}

	getPublicAccessDomain := func(zone string, publicAppIDs, publicCustomDomainApps []string, denied string) []string {
		r := []string{zone}
		if (publicAppIDs == nil && publicCustomDomainApps == nil) || denied != "1" {
			return r
		}
		for _, appID := range publicAppIDs {
			r = append(r, appID+"."+zone)
		}
		r = append(r, publicCustomDomainApps...)
		return r
	}

	type userSortable struct {
		info      *message.UserInfo
		timestamp int64
	}
	var sortable []userSortable

	for _, user := range userList {
		isEphemeralAnno := getAnnotation(&user, userAnnotationIsEphemeral)
		if !isValidUser(&user) && isEphemeralAnno == "" {
			continue
		}

		isEphemeral := false
		if ok, parseErr := strconv.ParseBool(isEphemeralAnno); parseErr == nil && ok {
			isEphemeral = true
		}

		var (
			did, zone, localDomainIP string
			accLevel, allowCIDR      string
			denyAllStatus            string
			allowedDomains           []string
			serverNameDomains        []string
		)

		if !isEphemeral {
			did = getAnnotation(&user, userAnnotationDid)
			zone = getAnnotation(&user, userAnnotationZone)
			localDomainIP = getAnnotation(&user, userLocalDomainIPDns)
			accLevel = getAnnotation(&user, userLauncherAccessLevel)
			allowCIDR = getAnnotation(&user, userLauncherAllowCIDR)
			serverNameDomains = []string{zone, user.Name + ".olares.local"}
			denyAllStatus = getAnnotation(&user, userDenyAllPolicy)
			allowedDomains = getPublicAccessDomain(zone, publicAppIDs, publicCustomDomainApps, denyAllStatus)

			if userCustomDomains, ok := customDomainAppsWithUsers[user.Name]; ok && len(userCustomDomains) > 0 {
				serverNameDomains = append(serverNameDomains, userCustomDomains...)
			}
		} else {
			creator := getAnnotation(&user, userAnnotationCreator)
			creatorUser := getUserByName(creator)
			if creatorUser == nil {
				klog.Warningf("provider: ephemeral user %q has no creator", user.Name)
				continue
			}
			did = getAnnotation(creatorUser, userAnnotationDid)
			zone = getAnnotation(creatorUser, userAnnotationZone)
			accLevel = getAnnotation(creatorUser, userLauncherAccessLevel)
			allowCIDR = getAnnotation(creatorUser, userLauncherAllowCIDR)
			denyAllStatus = getAnnotation(creatorUser, userDenyAllPolicy)
		}

		var accessLevel uint64
		if accLevel != "" {
			var err error
			accessLevel, err = strconv.ParseUint(accLevel, 10, 64)
			if err != nil {
				klog.Errorf("provider: user %q parse access level: %v", user.Name, err)
				continue
			}
		}

		denyAll, _ := strconv.Atoi(denyAllStatus)

		svcName := fmt.Sprintf("bfl.%s-%s", p.cfg.UserNamespacePrefix, user.Name)
		addr, err := lookupHostAddr(svcName)
		if err != nil {
			klog.V(2).Infof("provider: user %q lookup host: %v", user.Name, err)
			continue
		}

		info := &message.UserInfo{
			Name:              user.Name,
			Namespace:         fmt.Sprintf("%s-%s", p.cfg.UserNamespacePrefix, user.Name),
			Did:               did,
			Zone:              zone,
			IsEphemeral:       isEphemeral,
			BFLHost:           addr,
			BFLPort:           p.cfg.BFLServicePort,
			AccessLevel:       accessLevel,
			AllowCIDRs:        strings.Split(allowCIDR, ","),
			DenyAll:           denyAll == 1,
			AllowedDomains:    allowedDomains,
			ServerNameDomains: serverNameDomains,
			LocalDomainIP:     localDomainIP,
			CreateTimestamp:   user.CreationTimestamp.Unix(),
		}
		sortable = append(sortable, userSortable{info: info, timestamp: user.CreationTimestamp.Unix()})
	}

	sort.Slice(sortable, func(i, j int) bool {
		return sortable[i].timestamp > sortable[j].timestamp
	})

	result := make([]*message.UserInfo, 0, len(sortable))
	for _, s := range sortable {
		result = append(result, s.info)
	}
	return result, nil
}

// listApplicationDetails mirrors the original listApplications, returning public app IDs,
// public custom domain apps, all custom domain apps, and per-user custom domain mapping.
func (p *Provider) listApplicationDetails() ([]string, []string, []string, map[string][]string) {
	publicApps := []string{"headscale"}
	var publicCustomDomainApps []string
	var customDomainApps []string
	customDomainAppsWithUsers := make(map[string][]string)

	appList := p.getAppsFromCache()

	getAppPrefix := func(entranceCount, index int, appid string) string {
		if entranceCount == 1 {
			return appid
		}
		return fmt.Sprintf("%s%d", appid, index)
	}

	for _, app := range appList {
		if len(app.Spec.Entrances) == 0 {
			continue
		}

		var customDomains []string
		var customDomainsPrefix []string
		entranceCount := len(app.Spec.Entrances)
		owner := app.Spec.Owner

		for index, entrance := range app.Spec.Entrances {
			prefix := getAppPrefix(entranceCount, index, app.Spec.Appid)
			customDomainEntrancesMap := getSettingsKeyMap(&app, settingsCustomDomain)
			authLevel := entrance.AuthLevel

			if cdEntrance, ok := customDomainEntrancesMap[entrance.Name]; ok {
				if entrancePrefix := cdEntrance[settingsCustomDomainThirdLevelDomain]; entrancePrefix != "" {
					if authLevel == applicationAuthLevelPublic {
						customDomainsPrefix = append(customDomainsPrefix, entrancePrefix)
					}
				}
				if entranceCustomDomain := cdEntrance[settingsCustomDomainThirdPartyDomain]; entranceCustomDomain != "" {
					customDomainApps = append(customDomainApps, entranceCustomDomain)

					val := customDomainAppsWithUsers[owner]
					customDomainAppsWithUsers[owner] = append(val, entranceCustomDomain)

					if authLevel == applicationAuthLevelPublic {
						customDomains = append(customDomains, entranceCustomDomain)
					}
				}
			}

			if prefix != "" {
				if authLevel == applicationAuthLevelPublic {
					publicApps = append(publicApps, prefix)
				}
				if len(customDomainsPrefix) > 0 {
					publicApps = append(publicApps, customDomainsPrefix...)
				}
				if len(customDomains) > 0 {
					publicCustomDomainApps = append(publicCustomDomainApps, customDomains...)
				}
			}
		}
	}

	return publicApps, publicCustomDomainApps, customDomainApps, customDomainAppsWithUsers
}

func (p *Provider) getAppsFromCache() []appv2alpha1.Application {
	items := p.appStore.List()
	apps := make([]appv2alpha1.Application, 0, len(items))
	for _, item := range items {
		obj, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		data, err := obj.MarshalJSON()
		if err != nil {
			continue
		}
		var app appv2alpha1.Application
		if err = json.Unmarshal(data, &app); err != nil {
			continue
		}
		apps = append(apps, app)
	}
	return apps
}

func (p *Provider) getUsersFromCache() []iamv1alpha2.User {
	items := p.userStore.List()
	users := make([]iamv1alpha2.User, 0, len(items))
	for _, item := range items {
		obj, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		data, err := obj.MarshalJSON()
		if err != nil {
			continue
		}
		var user iamv1alpha2.User
		if err = json.Unmarshal(data, &user); err != nil {
			continue
		}
		users = append(users, user)
	}
	return users
}

func getAnnotation(user *iamv1alpha2.User, key string) string {
	if v, ok := user.Annotations[key]; ok && v != "" {
		return v
	}
	return ""
}

func isValidUser(user *iamv1alpha2.User) bool {
	return getAnnotation(user, userAnnotationDid) != "" && getAnnotation(user, userAnnotationZone) != ""
}

func getSettingsKeyMap(app *appv2alpha1.Application, key string) map[string]map[string]string {
	r := make(map[string]map[string]string)
	if app.Spec.Settings == nil {
		return r
	}
	data := app.Spec.Settings[key]
	if data == "" {
		return r
	}
	_ = json.Unmarshal([]byte(data), &r)
	return r
}

func lookupHostAddr(svc string) (string, error) {
	for i := 0; i < dnsLookupRetry; i++ {
		addrs, err := net.LookupHost(svc)
		if err != nil {
			klog.V(4).Infof("lookup %s: %v", svc, err)
			time.Sleep(dnsRetryBackoff)
			continue
		}
		if len(addrs) >= 1 {
			return addrs[0], nil
		}
	}
	return "", fmt.Errorf("svc %s: no host resolved", svc)
}
