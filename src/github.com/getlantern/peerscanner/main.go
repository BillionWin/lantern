// main simply contains the primary web serving code that allows peers to
// register and unregister as give mode peers running within the Lantern
// network
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/getlantern/cloudflare"
	"github.com/getlantern/golog"
	"github.com/getlantern/peerscanner/cf"
)

const (
	RoundRobin = "atest_roundrobin"
	Peers      = "atest_apeers"
	Fallbacks  = "atest_fallbacks"
)

var (
	log = golog.LoggerFor("peerscanner")

	port     = flag.Int("port", 62443, "Port, defaults to 62443")
	cfdomain = flag.String("cfdomain", "getiantem.org", "CloudFlare domain, defaults to getiantem.org")
	cfuser   = os.Getenv("CF_USER")
	cfkey    = os.Getenv("CF_API_KEY")

	cfutil *cf.Util

	// Map of all hosts being tracked by us, keyed to the combination of
	// name+ip.  We use the combination of name+ip so that we can smoothly
	// handle hosts of a given name changing their ip.
	hosts      map[hostkey]*host
	hostsMutex sync.Mutex
)

func main() {
	parseFlags()
	connectToCloudFlare()

	var err error
	hosts, err = loadHosts()
	if err != nil {
		log.Fatal(err)
	}

	startHttp()
}

func parseFlags() {
	flag.Parse()
	if cfuser == "" {
		log.Fatal("Please specify a CF_USER environment variable")
	}
	if cfkey == "" {
		log.Fatal("Please specify a CF_API_KEY environment variable")
	}
}

func connectToCloudFlare() {
	var err error
	cfutil, err = cf.New(*cfdomain, cfuser, cfkey)
	if err != nil {
		log.Fatalf("Unable to create CloudFlare utility: %v", err)
	}
}

func getOrCreateHost(name string, ip string) *host {
	hostsMutex.Lock()
	defer hostsMutex.Unlock()

	key := hostkey{name, ip}
	h := hosts[key]
	if h == nil {
		h := newHost(key, nil)
		hosts[key] = h
		go h.run()
		return h
	}
	h.reset()
	return h
}

func getHost(name string, ip string) *host {
	hostsMutex.Lock()
	defer hostsMutex.Unlock()

	key := hostkey{name, ip}
	return hosts[key]
}

func removeHost(h *host) {
	hostsMutex.Lock()
	delete(hosts, h.key)
	defer hostsMutex.Unlock()
}

func loadHosts() (map[hostkey]*host, error) {
	recs, err := cfutil.GetAllRecords()
	if err != nil {
		return nil, fmt.Errorf("Unable to load hosts: %v", err)
	}

	groups := map[string]map[string]*cloudflare.Record{
		RoundRobin: make(map[string]*cloudflare.Record),
		Fallbacks:  make(map[string]*cloudflare.Record),
		Peers:      make(map[string]*cloudflare.Record),
	}
	hosts := make(map[hostkey]*host, 0)

	addHost := func(r cloudflare.Record) {
		key := hostkey{r.Name, r.Value}
		h := newHost(key, &r)
		hosts[h.key] = h
	}

	addToGroup := func(name string, r cloudflare.Record) {
		log.Tracef("Adding to %v: %v", name, r.Value)
		groups[name][r.Value] = &r
	}

	for _, r := range recs {
		// We just check the length of the subdomain here, which is the unique
		// peer GUID. While it's possible something else could have a subdomain
		// this long, it's unlikely.
		if isPeer(r.Name) {
			log.Tracef("Adding peer: %v", r.Name)
			addHost(r)
		} else if isFallback(r.Name) {
			log.Tracef("Adding fallback: %v", r.Name)
			addHost(r)
		} else if r.Name == RoundRobin {
			addToGroup(RoundRobin, r)
		} else if r.Name == Fallbacks {
			addToGroup(Fallbacks, r)
		} else if r.Name == Peers {
			addToGroup(Peers, r)
		} else {
			log.Tracef("Unrecognized record: %v", r.FullName)
		}
	}

	// Update hosts with rotation info
	for _, h := range hosts {
		for _, hg := range h.groups {
			g, found := groups[hg.subdomain]
			if found {
				hg.existing = g[h.key.ip]
				delete(g, h.key.ip)
			}
		}
	}

	// Remove items from rotation that don't have a corresponding host
	var wg sync.WaitGroup
	for k, g := range groups {
		for _, r := range g {
			wg.Add(1)
			go removeFromRotation(&wg, k, r)
		}
	}
	wg.Wait()

	// Start hosts
	for _, h := range hosts {
		h.run()
	}

	return hosts, nil
}

func removeFromRotation(wg *sync.WaitGroup, k string, r *cloudflare.Record) {
	log.Debugf("%v in %v is missing host, removing from rotation", r.Value, k)
	err := cfutil.DestroyRecord(r)
	if err != nil {
		log.Debugf("Unable to remove %v from %v: %v", r.Value, k, err)
	}
	wg.Done()
}

func isPeer(name string) bool {
	// We just check the length of the subdomain here, which is the unique
	// peer GUID. While it's possible something else could have a subdomain
	// this long, it's unlikely.
	// We also accept anything with a name beginning with peer- as a peer
	return len(name) == 32 || strings.Index(name, "peer-") == 0
}

func isFallback(name string) bool {
	return strings.HasPrefix(name, "fl-")
}