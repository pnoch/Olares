package provider

import (
	"context"
	"encoding/json"
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
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

const (
	mapKey          = "default"
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
)

type Config struct {
	UserNamespacePrefix string
	BFLServicePort      int
	SSLServerPort       int
	SSLProxyServerPort  int
}

type Provider struct {
	cache      ctrlcache.Cache
	resources  *message.ProviderResources
	cfg        *Config
	synced     atomic.Bool
	debounceCh chan struct{}
}

func New(c ctrlcache.Cache, resources *message.ProviderResources, cfg *Config) *Provider {
	return &Provider{
		cache:      c,
		resources:  resources,
		cfg:        cfg,
		debounceCh: make(chan struct{}, 1),
	}
}

func (p *Provider) Name() string { return "provider" }

// SetupWithManager pre-registers informers and event handlers before the
// Manager starts. This ensures the cache includes User and Application
// informers in its initial sync, so cacheReadyCheck is accurate.
// Must be called before mgr.Start().
func (p *Provider) SetupWithManager(ctx context.Context) error {
	userInformer, err := p.cache.GetInformer(ctx, &iamv1alpha2.User{})
	if err != nil {
		return fmt.Errorf("get user informer: %w", err)
	}

	appInformer, err := p.cache.GetInformer(ctx, &appv2alpha1.Application{})
	if err != nil {
		return fmt.Errorf("get app informer: %w", err)
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { p.notifyChanged() },
		UpdateFunc: func(_, _ interface{}) { p.notifyChanged() },
		DeleteFunc: func(_ interface{}) { p.notifyChanged() },
	}
	if _, err := userInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("add user event handler failed: %w", err)
	}
	if _, err := appInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("add app event handler failed: %w", err)
	}

	klog.Info("provider: informers and event handlers registered")
	return nil
}

// Start is called by the Manager after the cache has synced.
// Informers are already registered and synced via SetupWithManager.
func (p *Provider) Start(ctx context.Context) error {
	p.synced.Store(true)
	klog.Info("provider: cache synced, publishing initial snapshot")
	p.publishResources(ctx)

	p.debounceLoop(ctx)
	klog.Info("provider: stopped")
	return nil
}

const debounceInterval = 100 * time.Millisecond

func (p *Provider) notifyChanged() {
	if !p.synced.Load() {
		return
	}
	select {
	case p.debounceCh <- struct{}{}:
	default:
	}
}

func (p *Provider) debounceLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.debounceCh:
			timer := time.NewTimer(debounceInterval)
		drain:
			for {
				select {
				case <-p.debounceCh:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(debounceInterval)
				case <-timer.C:
					break drain
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			p.publishResources(ctx)
		}
	}
}

func (p *Provider) publishResources(ctx context.Context) {
	rawApps := p.getAppsFromCache(ctx)

	users, err := p.listUsers(ctx, rawApps)
	if err != nil {
		klog.Errorf("provider: list users: %v", err)
		return
	}

	snapshot := &message.Resources{
		Users: users,
		Apps:  p.buildAppInfos(rawApps),
	}
	snapshot.Sort()

	if old, ok := p.resources.Load(mapKey); ok && old.Equal(snapshot) {
		klog.V(4).Info("provider: snapshot unchanged, skipping publish")
		return
	}

	p.resources.Store(mapKey, snapshot)
	klog.Infof("provider: published snapshot with %d users and %d apps", len(snapshot.Users), len(snapshot.Apps))
}

func (p *Provider) buildAppInfos(appList []appv2alpha1.Application) []*message.AppInfo {
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
	return result
}

func (p *Provider) listUsers(ctx context.Context, rawApps []appv2alpha1.Application) ([]*message.UserInfo, error) {
	publicAppIDs, publicCustomDomainApps, _, customDomainAppsWithUsers := p.listApplicationDetails(rawApps)

	userList := p.getUsersFromCache(ctx)

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

	var result []*message.UserInfo

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

		cidrs := strings.Split(allowCIDR, ",")
		sort.Strings(cidrs)
		sort.Strings(allowedDomains)
		sort.Strings(serverNameDomains)

		info := &message.UserInfo{
			Name:              user.Name,
			Namespace:         fmt.Sprintf("%s-%s", p.cfg.UserNamespacePrefix, user.Name),
			Did:               did,
			Zone:              zone,
			IsEphemeral:       isEphemeral,
			BFLHost:           addr,
			BFLPort:           p.cfg.BFLServicePort,
			AccessLevel:       accessLevel,
			AllowCIDRs:        cidrs,
			DenyAll:           denyAll == 1,
			AllowedDomains:    allowedDomains,
			ServerNameDomains: serverNameDomains,
			LocalDomainIP:     localDomainIP,
			CreateTimestamp:   user.CreationTimestamp.Unix(),
		}
		result = append(result, info)
	}

	return result, nil
}

func (p *Provider) listApplicationDetails(appList []appv2alpha1.Application) ([]string, []string, []string, map[string][]string) {
	publicApps := []string{"headscale"}
	var publicCustomDomainApps []string
	var customDomainApps []string
	customDomainAppsWithUsers := make(map[string][]string)

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

func (p *Provider) getAppsFromCache(ctx context.Context) []appv2alpha1.Application {
	var appList appv2alpha1.ApplicationList
	if err := p.cache.List(ctx, &appList); err != nil {
		klog.Errorf("provider: list apps from cache: %v", err)
		return nil
	}
	apps := appList.Items
	sort.Slice(apps, func(i, j int) bool {
		return apps[i].Spec.Name < apps[j].Spec.Name
	})
	return apps
}

func (p *Provider) getUsersFromCache(ctx context.Context) []iamv1alpha2.User {
	var userList iamv1alpha2.UserList
	if err := p.cache.List(ctx, &userList); err != nil {
		klog.Errorf("provider: list users from cache: %v", err)
		return nil
	}
	users := userList.Items
	sort.Slice(users, func(i, j int) bool {
		return users[i].Name < users[j].Name
	})
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
