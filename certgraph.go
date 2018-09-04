package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lanrat/certgraph/driver"
	"github.com/lanrat/certgraph/driver/crtsh"
	"github.com/lanrat/certgraph/driver/google"
	"github.com/lanrat/certgraph/driver/http"
	"github.com/lanrat/certgraph/driver/smtp"
	"github.com/lanrat/certgraph/graph"
	"github.com/lanrat/certgraph/status"
)

var (
	gitDate   = "none"
	gitHash   = "master"
	certGraph = graph.NewCertGraph()
)

var certDriver driver.Driver

// config & flags
var config struct {
	timeout             time.Duration
	verbose             bool
	maxDepth            uint
	parallel            uint
	savePath            string
	details             bool
	printJSON           bool
	driver              string
	includeCTSubdomains bool
	includeCTExpired    bool
	cdn                 bool
	maxSANsSize         int
	tldPlus1            bool
	checkNS             bool
	printVersion        bool
}

func init() {
	var timeoutSeconds uint
	flag.BoolVar(&config.printVersion, "version", false, "print version and exit")
	flag.UintVar(&timeoutSeconds, "timeout", 10, "tcp timeout in seconds")
	flag.BoolVar(&config.verbose, "verbose", false, "verbose logging")
	flag.StringVar(&config.driver, "driver", "http", fmt.Sprintf("driver to use [%s]", strings.Join(driver.Drivers, ", ")))
	flag.BoolVar(&config.includeCTSubdomains, "ct-subdomains", false, "include sub-domains in certificate transparency search")
	flag.BoolVar(&config.includeCTExpired, "ct-expired", false, "include expired certificates in certificate transparency search")
	flag.IntVar(&config.maxSANsSize, "sanscap", 80, "maximum number of uniq TLD+1 domains in certificate to include, 0 has no limit")
	flag.BoolVar(&config.cdn, "cdn", false, "include certificates from CDNs")
	flag.BoolVar(&config.checkNS, "ns", false, "check for NS records to determine if domain is registered")
	flag.BoolVar(&config.tldPlus1, "tldplus1", false, "for every domain found, add tldPlus1 of the domain's parent")
	flag.UintVar(&config.maxDepth, "depth", 5, "maximum BFS depth to go")
	flag.UintVar(&config.parallel, "parallel", 10, "number of certificates to retrieve in parallel")
	flag.BoolVar(&config.details, "details", false, "print details about the domains crawled")
	flag.BoolVar(&config.printJSON, "json", false, "print the graph as json, can be used for graph in web UI")
	flag.StringVar(&config.savePath, "save", "", "save certs to folder in PEM format")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s: [OPTION]... HOST...\n\thttps://github.com/lanrat/certgraph\nOPTIONS:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	config.timeout = time.Duration(timeoutSeconds) * time.Second
}

func main() {
	// check for version flag
	if config.printVersion {
		fmt.Println(version())
		return
	}

	// print usage if no domain passed
	if flag.NArg() < 1 {
		flag.Usage()
		return
	}

	// cant run on 0 threads
	if config.parallel < 1 {
		fmt.Fprintln(os.Stderr, "Must enter a positive number of parallel threads")
		flag.Usage()
		return
	}

	// add domains passed to startDomains
	startDomains := make([]string, 0, 1)
	for _, domain := range flag.Args() {
		d := strings.ToLower(domain)
		if len(d) > 0 {
			startDomains = append(startDomains, cleanInput(d))
			if config.tldPlus1 {
				tldPlus1, err := status.TLDPlus1(domain)
				if err != nil {
					continue
				}
				startDomains = append(startDomains, tldPlus1)
			}
		}
	}

	// set driver
	err := setDriver(config.driver)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// create the output directory if it does not exist
	if len(config.savePath) > 0 {
		err := os.MkdirAll(config.savePath, 0777)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}

	// perform breath-first-search on the graph
	breathFirstSearch(startDomains)

	// print the json output
	if config.printJSON {
		printJSONGraph()
	}

	v("Found", certGraph.NumDomains(), "domains")
	v("Graph Depth:", certGraph.DomainDepth())
}

// setDriver sets the driver variable for the provided driver string and does any necessary driver prep work
// TODO make config generic and move this to driver module
func setDriver(driver string) error {
	var err error
	switch driver {
	case "google":
		certDriver, err = google.Driver(50, config.savePath, config.includeCTSubdomains, config.includeCTExpired)
	case "crtsh":
		certDriver, err = crtsh.Driver(1000, config.timeout, config.savePath, config.includeCTSubdomains, config.includeCTExpired)
	case "http":
		certDriver, err = http.Driver(config.timeout, config.savePath)
	case "smtp":
		certDriver, err = smtp.Driver(config.timeout, config.savePath)
	default:
		return fmt.Errorf("Unknown driver name: %s", config.driver)
	}
	return err
}

// verbose logging
func v(a ...interface{}) {
	if config.verbose {
		e(a...)
	}
}

func e(a ...interface{}) {
	fmt.Fprintln(os.Stderr, a...)
}

// prints the graph as a json object
func printJSONGraph() {
	jsonGraph := certGraph.GenerateMap()
	jsonGraph["certgraph"] = generateGraphMetadata()

	j, err := json.MarshalIndent(jsonGraph, "", "\t")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(j))
}

// breathFirstSearch perform Breadth first search to build the graph
func breathFirstSearch(roots []string) {
	var wg sync.WaitGroup
	domainNodeInputChan := make(chan *graph.DomainNode, 5)  // input queue
	domainNodeOutputChan := make(chan *graph.DomainNode, 5) // output queue

	// thread limit code
	threadPass := make(chan bool, config.parallel)
	for i := uint(0); i < config.parallel; i++ {
		threadPass <- true
	}

	// thread to put root nodes/domains into queue
	wg.Add(1)
	go func() {
		// the waitGroup Add and Done for this thread ensures that we don't exit before any of the inputs domains are put into the Queue
		defer wg.Done()
		for _, root := range roots {
			wg.Add(1)
			n := graph.NewDomainNode(root, 0)
			n.Root = true
			domainNodeInputChan <- n
		}
	}()
	// thread to start all other threads from DomainChan
	go func() {
		for {
			domainNode := <-domainNodeInputChan

			// depth check
			if domainNode.Depth > config.maxDepth {
				v("Max depth reached, skipping:", domainNode.Domain)
				wg.Done()
				continue
			}
			// use certGraph.domains map as list of
			// domains that are queued to be visited, or already have been

			if _, found := certGraph.GetDomain(domainNode.Domain); !found {
				certGraph.AddDomain(domainNode)
				go func(domainNode *graph.DomainNode) {
					defer wg.Done()
					// wait for pass
					<-threadPass
					defer func() { threadPass <- true }()

					// operate on the node
					v("Visiting", domainNode.Depth, domainNode.Domain)
					visit(domainNode)
					domainNodeOutputChan <- domainNode
					for _, neighbor := range certGraph.GetDomainNeighbors(domainNode.Domain, config.cdn, config.maxSANsSize) {
						wg.Add(1)
						domainNodeInputChan <- graph.NewDomainNode(neighbor, domainNode.Depth+1)
						if config.tldPlus1 {
							tldPlus1, err := status.TLDPlus1(neighbor)
							if err != nil {
								continue
							}
							wg.Add(1)
							domainNodeInputChan <- graph.NewDomainNode(tldPlus1, domainNode.Depth+1)
						}
					}
				}(domainNode)
			} else {
				wg.Done()
			}
		}
	}()

	// save/output thread
	done := make(chan bool)
	go func() {
		for {
			domainNode, more := <-domainNodeOutputChan
			if more {
				if !config.printJSON {
					if config.details {
						fmt.Fprintln(os.Stdout, domainNode)
					} else {
						fmt.Fprintln(os.Stdout, domainNode.Domain)
					}
					if config.checkNS {
						// TODO these ns lookups are likely done a LOT for many subdomains of the same domain
						ns, err := status.HasNameservers(domainNode.Domain, config.timeout)
						if err != nil {
							v("NS check error:", domainNode.Domain, err)
							continue
						}
						if !ns {
							// TODO print tldplus1 in a good way
							fmt.Fprintf(os.Stdout, "Missing NS: %s\n", domainNode.Domain)
						}
					}
				} else if config.details {
					fmt.Fprintln(os.Stderr, domainNode)
				}
			} else {
				done <- true
				return
			}
		}
	}()

	wg.Wait() // wait for querying to finish
	close(domainNodeOutputChan)
	<-done // wait for save to finish
}

// visit visit each node and get and set its neighbors
func visit(domainNode *graph.DomainNode) {
	// perform cert search
	// TODO do pagination in multiple threads to not block on long searches
	results, err := certDriver.QueryDomain(domainNode.Domain)
	if err != nil {
		// this is VERY common to error, usually this is a DNS or tcp connection related issue
		// we will skip the domain if we can't query it
		v("QueryDomain", domainNode.Domain, err)
		return
	}
	statuses := results.GetStatus()
	domainNode.AddStatusMap(statuses)
	relatedDomains, err := results.GetRelated()
	if err != nil {
		v("GetRelated", domainNode.Domain, err)
		return
	}
	domainNode.AddRelatedDomains(relatedDomains)

	// TODO parallelize this
	// TODO fix printing domains as they are found with new driver
	// add cert nodes to graph
	fingerprintMap, err := results.GetFingerprints()
	if err != nil {
		v("GetFingerprints", err)
		return
	}

	// fingerprints for the domain queried
	fingerprints := fingerprintMap[domainNode.Domain]
	for _, fp := range fingerprints {
		// add certnode to graph
		certNode, exists := certGraph.GetCert(fp)
		if !exists {
			// get cert details
			certResult, err := results.QueryCert(fp)
			if err != nil {
				v("QueryCert", err)
				continue
			}

			certNode = certNodeFromCertResult(certResult)
			certGraph.AddCert(certNode)
		}

		certNode.AddFound(certDriver.GetName())
		domainNode.AddCertFingerprint(certNode.Fingerprint, certDriver.GetName())
	}

	// we dont process any other certificates returned, they will be collected
	//  when we process the related domains
}

// certNodeFromCertResult convert certResult to certNode
func certNodeFromCertResult(certResult *driver.CertResult) *graph.CertNode {
	certNode := &graph.CertNode{
		Fingerprint: certResult.Fingerprint,
		Domains:     certResult.Domains,
	}
	return certNode
}

// generates metadata for the JSON output
// TODO map all config json
func generateGraphMetadata() map[string]interface{} {
	data := make(map[string]interface{})
	data["version"] = version()
	data["website"] = "https://lanrat.github.io/certgraph/"
	data["scan_date"] = time.Now().UTC()
	data["command"] = strings.Join(os.Args, " ")
	options := make(map[string]interface{})
	options["parallel"] = config.parallel
	options["driver"] = config.driver
	options["ct_subdomains"] = config.includeCTSubdomains
	options["ct_expired"] = config.includeCTExpired
	options["sanscap"] = config.maxSANsSize
	options["cdn"] = config.cdn
	options["timeout"] = config.timeout
	data["options"] = options
	return data
}

// returns the version string
func version() string {
	return fmt.Sprintf("Git commit: %s [%s]", gitDate, gitHash)
}

// cleanInput attempts to parse the input string as a url to extract the hostname
// if it fails, then the input string is returned
// also removes tailing '.'
func cleanInput(host string) string {
	host = strings.TrimSuffix(host, ".")
	u, err := url.Parse(host)
	if err != nil {
		return host
	}
	hostname := u.Hostname()
	if hostname == "" {
		return host
	}
	return hostname
}
