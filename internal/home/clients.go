package home

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/aghnet"
	"github.com/AdguardTeam/AdGuardHome/internal/arpdb"
	"github.com/AdguardTeam/AdGuardHome/internal/client"
	"github.com/AdguardTeam/AdGuardHome/internal/dhcpsvc"
	"github.com/AdguardTeam/AdGuardHome/internal/dnsforward"
	"github.com/AdguardTeam/AdGuardHome/internal/filtering"
	"github.com/AdguardTeam/AdGuardHome/internal/querylog"
	"github.com/AdguardTeam/AdGuardHome/internal/schedule"
	"github.com/AdguardTeam/AdGuardHome/internal/whois"
	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/hostsfile"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/golibs/stringutil"
)

// DHCP is an interface for accessing DHCP lease data the [clientsContainer]
// needs.
type DHCP interface {
	// Leases returns all the DHCP leases.
	Leases() (leases []*dhcpsvc.Lease)

	// HostByIP returns the hostname of the DHCP client with the given IP
	// address.  The address will be netip.Addr{} if there is no such client,
	// due to an assumption that a DHCP client must always have a hostname.
	HostByIP(ip netip.Addr) (host string)

	// MACByIP returns the MAC address for the given IP address leased.  It
	// returns nil if there is no such client, due to an assumption that a DHCP
	// client must always have a MAC address.
	MACByIP(ip netip.Addr) (mac net.HardwareAddr)
}

// clientsContainer is the storage of all runtime and persistent clients.
type clientsContainer struct {
	// storage stores information about persistent clients.
	storage *client.Storage

	// dhcp is the DHCP service implementation.
	dhcp DHCP

	// clientChecker checks if a client is blocked by the current access
	// settings.
	clientChecker BlockedClientChecker

	// etcHosts contains list of rewrite rules taken from the operating system's
	// hosts database.
	etcHosts *aghnet.HostsContainer

	// arpDB stores the neighbors retrieved from ARP.
	arpDB arpdb.Interface

	// lock protects all fields.
	//
	// TODO(a.garipov): Use a pointer and describe which fields are protected in
	// more detail.  Use sync.RWMutex.
	lock sync.Mutex

	// safeSearchCacheSize is the size of the safe search cache to use for
	// persistent clients.
	safeSearchCacheSize uint

	// safeSearchCacheTTL is the TTL of the safe search cache to use for
	// persistent clients.
	safeSearchCacheTTL time.Duration

	// testing is a flag that disables some features for internal tests.
	//
	// TODO(a.garipov): Awful.  Remove.
	testing bool
}

// BlockedClientChecker checks if a client is blocked by the current access
// settings.
type BlockedClientChecker interface {
	IsBlockedClient(ip netip.Addr, clientID string) (blocked bool, rule string)
}

// Init initializes clients container
// dhcpServer: optional
// Note: this function must be called only once
func (clients *clientsContainer) Init(
	objects []*clientObject,
	dhcpServer DHCP,
	etcHosts *aghnet.HostsContainer,
	arpDB arpdb.Interface,
	filteringConf *filtering.Config,
) (err error) {
	// TODO(s.chzhen):  Refactor it.
	if clients.storage != nil {
		return errors.Error("clients container already initialized")
	}

	clients.storage = client.NewStorage(&client.Config{
		AllowedTags: clientTags,
	})

	// TODO(e.burkov):  Use [dhcpsvc] implementation when it's ready.
	clients.dhcp = dhcpServer

	clients.etcHosts = etcHosts
	clients.arpDB = arpDB
	err = clients.addFromConfig(objects, filteringConf)
	if err != nil {
		// Don't wrap the error, because it's informative enough as is.
		return err
	}

	clients.safeSearchCacheSize = filteringConf.SafeSearchCacheSize
	clients.safeSearchCacheTTL = time.Minute * time.Duration(filteringConf.CacheTime)

	if clients.testing {
		return nil
	}

	// The clients.etcHosts may be nil even if config.Clients.Sources.HostsFile
	// is true, because of the deprecated option --no-etc-hosts.
	//
	// TODO(e.burkov):  The option should probably be returned, since hosts file
	// currently used not only for clients' information enrichment, but also in
	// the filtering module and upstream addresses resolution.
	if config.Clients.Sources.HostsFile && clients.etcHosts != nil {
		go clients.handleHostsUpdates()
	}

	return nil
}

// handleHostsUpdates receives the updates from the hosts container and adds
// them to the clients container.  It is intended to be used as a goroutine.
func (clients *clientsContainer) handleHostsUpdates() {
	for upd := range clients.etcHosts.Upd() {
		clients.addFromHostsFile(upd)
	}
}

// webHandlersRegistered prevents a [clientsContainer] from registering its web
// handlers more than once.
//
// TODO(a.garipov): Refactor HTTP handler registration logic.
var webHandlersRegistered = false

// Start starts the clients container.
func (clients *clientsContainer) Start() {
	if clients.testing {
		return
	}

	if !webHandlersRegistered {
		webHandlersRegistered = true
		clients.registerWebHandlers()
	}

	go clients.periodicUpdate()
}

// reloadARP reloads runtime clients from ARP, if configured.
func (clients *clientsContainer) reloadARP() {
	if clients.arpDB != nil {
		clients.addFromSystemARP()
	}
}

// clientObject is the YAML representation of a persistent client.
type clientObject struct {
	SafeSearchConf filtering.SafeSearchConfig `yaml:"safe_search"`

	// BlockedServices is the configuration of blocked services of a client.
	BlockedServices *filtering.BlockedServices `yaml:"blocked_services"`

	Name string `yaml:"name"`

	IDs       []string `yaml:"ids"`
	Tags      []string `yaml:"tags"`
	Upstreams []string `yaml:"upstreams"`

	// UID is the unique identifier of the persistent client.
	UID client.UID `yaml:"uid"`

	// UpstreamsCacheSize is the DNS cache size (in bytes).
	//
	// TODO(d.kolyshev): Use [datasize.Bytesize].
	UpstreamsCacheSize uint32 `yaml:"upstreams_cache_size"`

	// UpstreamsCacheEnabled indicates if the DNS cache is enabled.
	UpstreamsCacheEnabled bool `yaml:"upstreams_cache_enabled"`

	UseGlobalSettings        bool `yaml:"use_global_settings"`
	FilteringEnabled         bool `yaml:"filtering_enabled"`
	ParentalEnabled          bool `yaml:"parental_enabled"`
	SafeBrowsingEnabled      bool `yaml:"safebrowsing_enabled"`
	UseGlobalBlockedServices bool `yaml:"use_global_blocked_services"`

	IgnoreQueryLog   bool `yaml:"ignore_querylog"`
	IgnoreStatistics bool `yaml:"ignore_statistics"`
}

// toPersistent returns an initialized persistent client if there are no errors.
func (o *clientObject) toPersistent(
	filteringConf *filtering.Config,
) (cli *client.Persistent, err error) {
	cli = &client.Persistent{
		Name: o.Name,

		Upstreams: o.Upstreams,

		UID: o.UID,

		UseOwnSettings:        !o.UseGlobalSettings,
		FilteringEnabled:      o.FilteringEnabled,
		ParentalEnabled:       o.ParentalEnabled,
		SafeSearchConf:        o.SafeSearchConf,
		SafeBrowsingEnabled:   o.SafeBrowsingEnabled,
		UseOwnBlockedServices: !o.UseGlobalBlockedServices,
		IgnoreQueryLog:        o.IgnoreQueryLog,
		IgnoreStatistics:      o.IgnoreStatistics,
		UpstreamsCacheEnabled: o.UpstreamsCacheEnabled,
		UpstreamsCacheSize:    o.UpstreamsCacheSize,
	}

	err = cli.SetIDs(o.IDs)
	if err != nil {
		return nil, fmt.Errorf("parsing ids: %w", err)
	}

	if (cli.UID == client.UID{}) {
		cli.UID, err = client.NewUID()
		if err != nil {
			return nil, fmt.Errorf("generating uid: %w", err)
		}
	}

	if o.SafeSearchConf.Enabled {
		err = cli.SetSafeSearch(
			o.SafeSearchConf,
			filteringConf.SafeSearchCacheSize,
			time.Minute*time.Duration(filteringConf.CacheTime),
		)
		if err != nil {
			return nil, fmt.Errorf("init safesearch %q: %w", cli.Name, err)
		}
	}

	if o.BlockedServices == nil {
		o.BlockedServices = &filtering.BlockedServices{
			Schedule: schedule.EmptyWeekly(),
		}
	}

	err = o.BlockedServices.Validate()
	if err != nil {
		return nil, fmt.Errorf("init blocked services %q: %w", cli.Name, err)
	}

	cli.BlockedServices = o.BlockedServices.Clone()

	cli.Tags = slices.Clone(o.Tags)

	return cli, nil
}

// addFromConfig initializes the clients container with objects from the
// configuration file.
func (clients *clientsContainer) addFromConfig(
	objects []*clientObject,
	filteringConf *filtering.Config,
) (err error) {
	for i, o := range objects {
		var cli *client.Persistent
		cli, err = o.toPersistent(filteringConf)
		if err != nil {
			return fmt.Errorf("clients: init persistent client at index %d: %w", i, err)
		}

		err = clients.storage.Add(cli)
		if err != nil {
			return fmt.Errorf("adding client %q at index %d: %w", cli.Name, i, err)
		}
	}

	return nil
}

// forConfig returns all currently known persistent clients as objects for the
// configuration file.
func (clients *clientsContainer) forConfig() (objs []*clientObject) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	objs = make([]*clientObject, 0, clients.storage.Size())
	clients.storage.RangeByName(func(cli *client.Persistent) (cont bool) {
		objs = append(objs, &clientObject{
			Name: cli.Name,

			BlockedServices: cli.BlockedServices.Clone(),

			IDs:       cli.IDs(),
			Tags:      slices.Clone(cli.Tags),
			Upstreams: slices.Clone(cli.Upstreams),

			UID: cli.UID,

			UseGlobalSettings:        !cli.UseOwnSettings,
			FilteringEnabled:         cli.FilteringEnabled,
			ParentalEnabled:          cli.ParentalEnabled,
			SafeSearchConf:           cli.SafeSearchConf,
			SafeBrowsingEnabled:      cli.SafeBrowsingEnabled,
			UseGlobalBlockedServices: !cli.UseOwnBlockedServices,
			IgnoreQueryLog:           cli.IgnoreQueryLog,
			IgnoreStatistics:         cli.IgnoreStatistics,
			UpstreamsCacheEnabled:    cli.UpstreamsCacheEnabled,
			UpstreamsCacheSize:       cli.UpstreamsCacheSize,
		})

		return true
	})

	return objs
}

// arpClientsUpdatePeriod defines how often ARP clients are updated.
const arpClientsUpdatePeriod = 10 * time.Minute

func (clients *clientsContainer) periodicUpdate() {
	defer log.OnPanic("clients container")

	for {
		clients.reloadARP()
		time.Sleep(arpClientsUpdatePeriod)
	}
}

// clientSource checks if client with this IP address already exists and returns
// the source which updated it last.  It returns [client.SourceNone] if the
// client doesn't exist.  Note that it is only used in tests.
func (clients *clientsContainer) clientSource(ip netip.Addr) (src client.Source) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	_, ok := clients.findLocked(ip.String())
	if ok {
		return client.SourcePersistent
	}

	rc := clients.storage.ClientRuntime(ip)
	if rc != nil {
		src, _ = rc.Info()
	}

	if src < client.SourceDHCP && clients.dhcp.HostByIP(ip) != "" {
		src = client.SourceDHCP
	}

	return src
}

// findMultiple is a wrapper around [clientsContainer.find] to make it a valid
// client finder for the query log.  c is never nil; if no information about the
// client is found, it returns an artificial client record by only setting the
// blocking-related fields.  err is always nil.
func (clients *clientsContainer) findMultiple(ids []string) (c *querylog.Client, err error) {
	var artClient *querylog.Client
	var art bool
	for _, id := range ids {
		ip, _ := netip.ParseAddr(id)
		c, art = clients.clientOrArtificial(ip, id)
		if art {
			artClient = c

			continue
		}

		return c, nil
	}

	return artClient, nil
}

// clientOrArtificial returns information about one client.  If art is true,
// this is an artificial client record, meaning that we currently don't have any
// records about this client besides maybe whether or not it is blocked.  c is
// never nil.
func (clients *clientsContainer) clientOrArtificial(
	ip netip.Addr,
	id string,
) (c *querylog.Client, art bool) {
	defer func() {
		c.Disallowed, c.DisallowedRule = clients.clientChecker.IsBlockedClient(ip, id)
		if c.WHOIS == nil {
			c.WHOIS = &whois.Info{}
		}
	}()

	cli, ok := clients.storage.FindLoose(ip, id)
	if ok {
		return &querylog.Client{
			Name:           cli.Name,
			IgnoreQueryLog: cli.IgnoreQueryLog,
		}, false
	}

	rc := clients.findRuntimeClient(ip)
	if rc != nil {
		_, host := rc.Info()

		return &querylog.Client{
			Name:  host,
			WHOIS: rc.WHOIS(),
		}, false
	}

	return &querylog.Client{
		Name: "",
	}, true
}

// find returns a shallow copy of the client if there is one found.
func (clients *clientsContainer) find(id string) (c *client.Persistent, ok bool) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	c, ok = clients.findLocked(id)
	if !ok {
		return nil, false
	}

	return c, true
}

// shouldCountClient is a wrapper around [clientsContainer.find] to make it a
// valid client information finder for the statistics.  If no information about
// the client is found, it returns true.
func (clients *clientsContainer) shouldCountClient(ids []string) (y bool) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	for _, id := range ids {
		client, ok := clients.findLocked(id)
		if ok {
			return !client.IgnoreStatistics
		}
	}

	return true
}

// type check
var _ dnsforward.ClientsContainer = (*clientsContainer)(nil)

// UpstreamConfigByID implements the [dnsforward.ClientsContainer] interface for
// *clientsContainer.  upsConf is nil if the client isn't found or if the client
// has no custom upstreams.
func (clients *clientsContainer) UpstreamConfigByID(
	id string,
	bootstrap upstream.Resolver,
) (conf *proxy.CustomUpstreamConfig, err error) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	c, ok := clients.findLocked(id)
	if !ok {
		return nil, nil
	} else if c.UpstreamConfig != nil {
		return c.UpstreamConfig, nil
	}

	upstreams := stringutil.FilterOut(c.Upstreams, dnsforward.IsCommentOrEmpty)
	if len(upstreams) == 0 {
		return nil, nil
	}

	var upsConf *proxy.UpstreamConfig
	upsConf, err = proxy.ParseUpstreamsConfig(
		upstreams,
		&upstream.Options{
			Bootstrap:    bootstrap,
			Timeout:      config.DNS.UpstreamTimeout.Duration,
			HTTPVersions: dnsforward.UpstreamHTTPVersions(config.DNS.UseHTTP3Upstreams),
			PreferIPv6:   config.DNS.BootstrapPreferIPv6,
		},
	)
	if err != nil {
		// Don't wrap the error since it's informative enough as is.
		return nil, err
	}

	conf = proxy.NewCustomUpstreamConfig(
		upsConf,
		c.UpstreamsCacheEnabled,
		int(c.UpstreamsCacheSize),
		config.DNS.EDNSClientSubnet.Enabled,
	)
	c.UpstreamConfig = conf

	return conf, nil
}

// findLocked searches for a client by its ID.  clients.lock is expected to be
// locked.
func (clients *clientsContainer) findLocked(id string) (c *client.Persistent, ok bool) {
	c, ok = clients.storage.Find(id)
	if ok {
		return c, true
	}

	ip, err := netip.ParseAddr(id)
	if err != nil {
		return nil, false
	}

	// TODO(e.burkov):  Iterate through clients.list only once.
	return clients.findDHCP(ip)
}

// findDHCP searches for a client by its MAC, if the DHCP server is active and
// there is such client.  clients.lock is expected to be locked.
func (clients *clientsContainer) findDHCP(ip netip.Addr) (c *client.Persistent, ok bool) {
	foundMAC := clients.dhcp.MACByIP(ip)
	if foundMAC == nil {
		return nil, false
	}

	return clients.storage.FindByMAC(foundMAC)
}

// findRuntimeClient finds a runtime client by their IP.
func (clients *clientsContainer) findRuntimeClient(ip netip.Addr) (rc *client.Runtime) {
	rc = clients.storage.ClientRuntime(ip)
	host := clients.dhcp.HostByIP(ip)

	if host != "" {
		if rc == nil {
			rc = client.NewRuntime(ip)
		}

		rc.SetInfo(client.SourceDHCP, []string{host})

		return rc
	}

	return rc
}

// setWHOISInfo sets the WHOIS information for a client.  clients.lock is
// expected to be locked.
func (clients *clientsContainer) setWHOISInfo(ip netip.Addr, wi *whois.Info) {
	_, ok := clients.findLocked(ip.String())
	if ok {
		log.Debug("clients: client for %s is already created, ignore whois info", ip)

		return
	}

	rc := client.NewRuntime(ip)
	rc.SetWHOIS(wi)
	clients.storage.UpdateRuntime(rc)

	log.Debug("clients: set whois info for runtime client with ip %s: %+v", ip, wi)
}

// addHost adds a new IP-hostname pairing.  The priorities of the sources are
// taken into account.  ok is true if the pairing was added.
//
// TODO(a.garipov): Only used in internal tests.  Consider removing.
func (clients *clientsContainer) addHost(
	ip netip.Addr,
	host string,
	src client.Source,
) (ok bool) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	return clients.addHostLocked(ip, host, src)
}

// type check
var _ client.AddressUpdater = (*clientsContainer)(nil)

// UpdateAddress implements the [client.AddressUpdater] interface for
// *clientsContainer
func (clients *clientsContainer) UpdateAddress(ip netip.Addr, host string, info *whois.Info) {
	// Common fast path optimization.
	if host == "" && info == nil {
		return
	}

	clients.lock.Lock()
	defer clients.lock.Unlock()

	if host != "" {
		ok := clients.addHostLocked(ip, host, client.SourceRDNS)
		if !ok {
			log.Debug("clients: host for client %q already set with higher priority source", ip)
		}
	}

	if info != nil {
		clients.setWHOISInfo(ip, info)
	}
}

// addHostLocked adds a new IP-hostname pairing.  clients.lock is expected to be
// locked.
func (clients *clientsContainer) addHostLocked(
	ip netip.Addr,
	host string,
	src client.Source,
) (ok bool) {
	rc := client.NewRuntime(ip)
	rc.SetInfo(src, []string{host})
	if dhcpHost := clients.dhcp.HostByIP(ip); dhcpHost != "" {
		rc.SetInfo(client.SourceDHCP, []string{dhcpHost})
	}

	clients.storage.UpdateRuntime(rc)

	log.Debug(
		"clients: adding client info %s -> %q %q [%d]",
		ip,
		src,
		host,
		clients.storage.SizeRuntime(),
	)

	return true
}

// addFromHostsFile fills the client-hostname pairing index from the system's
// hosts files.
func (clients *clientsContainer) addFromHostsFile(hosts *hostsfile.DefaultStorage) {
	clients.lock.Lock()
	defer clients.lock.Unlock()

	var rcs []*client.Runtime
	hosts.RangeNames(func(addr netip.Addr, names []string) (cont bool) {
		// Only the first name of the first record is considered a canonical
		// hostname for the IP address.
		//
		// TODO(e.burkov):  Consider using all the names from all the records.
		rc := client.NewRuntime(addr)
		rc.SetInfo(client.SourceHostsFile, []string{names[0]})

		rcs = append(rcs, rc)

		return true
	})

	added, removed := clients.storage.BatchUpdateBySource(client.SourceHostsFile, rcs)
	log.Debug("clients: added %d, removed %d client aliases from system hosts file", added, removed)
}

// addFromSystemARP adds the IP-hostname pairings from the output of the arp -a
// command.
func (clients *clientsContainer) addFromSystemARP() {
	if err := clients.arpDB.Refresh(); err != nil {
		log.Error("refreshing arp container: %s", err)

		clients.arpDB = arpdb.Empty{}

		return
	}

	ns := clients.arpDB.Neighbors()
	if len(ns) == 0 {
		log.Debug("refreshing arp container: the update is empty")

		return
	}

	clients.lock.Lock()
	defer clients.lock.Unlock()

	var rcs []*client.Runtime
	for _, n := range ns {
		rc := client.NewRuntime(n.IP)
		rc.SetInfo(client.SourceARP, []string{n.Name})

		rcs = append(rcs, rc)
	}

	added, removed := clients.storage.BatchUpdateBySource(client.SourceARP, rcs)
	log.Debug("clients: added %d, removed %d client aliases from arp neighborhood", added, removed)
}

// close gracefully closes all the client-specific upstream configurations of
// the persistent clients.
func (clients *clientsContainer) close() (err error) {
	return clients.storage.CloseUpstreams()
}
