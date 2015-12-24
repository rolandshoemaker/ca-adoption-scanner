package stats

import (
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"

	"github.com/rolandshoemaker/ctat/common"
	"github.com/rolandshoemaker/ctat/filter"

	ct "github.com/jsha/certificatetransparency"
	"golang.org/x/net/publicsuffix"
)

type intBucket struct {
	value int
	count int
}

type intDistribution []intBucket

func (d intDistribution) Len() int           { return len(d) }
func (d intDistribution) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }
func (d intDistribution) Less(i, j int) bool { return d[i].value < d[j].value }

func (d intDistribution) print(valueLabel string, sum int) {
	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintf(w, "Count\t\t%s\t \n", valueLabel)
	maxWidth := 100.0
	for _, b := range d {
		percent := float64(b.count) / float64(sum)
		fmt.Fprintf(w, "%d\t%.4f%%\t%d\t%s\n", b.count, percent*100.0, b.value, strings.Repeat("*", int(maxWidth*percent)))
	}
	w.Flush()
}

type strBucket struct {
	value string
	count int
}

type strDistribution []strBucket

func (d strDistribution) Len() int           { return len(d) }
func (d strDistribution) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }
func (d strDistribution) Less(i, j int) bool { return d[i].count > d[j].count }

func (d strDistribution) print(valueLabel string, sum int) {
	w := new(tabwriter.Writer)
	w.Init(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintf(w, "Count\t\t%s\t \n", valueLabel)
	maxWidth := 100.0
	for _, b := range d {
		percent := float64(b.count) / float64(sum)
		fmt.Fprintf(w, "%d\t%.4f%%\t%s\t%s\n", b.count, percent*100.0, b.value, strings.Repeat("*", int(maxWidth*percent)))
	}
	w.Flush()
}

type metricGenerator interface {
	process(*x509.Certificate)
	print()
}

var metricsLookup = map[string]metricGenerator{
	"validityDist":      &validityDistribution{periods: make(map[int]int)},
	"certSizeDist":      &certSizeDistribution{sizes: make(map[int]int)},
	"nameMetrics":       &nameMetrics{names: make(map[string]int), nameSets: make(map[string]int)},
	"sanSizeDist":       &sanSizeDistribution{sizes: make(map[int]int)},
	"pkTypeDist":        &pkAlgDistribution{algs: make(map[string]int)},
	"sigTypeDist":       &sigAlgDistribution{algs: make(map[string]int)},
	"popularSuffixes":   &popularSuffixes{suffixes: make(map[string]int)},
	"leafIssuers":       &leafIssuanceDist{issuances: make(map[string]int)},
	"serialLengthDist":  &serialLengthDistribution{lengths: make(map[int]int)},
	"keyUsageDist":      &keyUsageDist{usage: make(map[string]int)},
	"featureMetrics":    &featureMetrics{features: make(map[string]int)},
	"numExtensionsDist": &numExtensionsDistribution{extensions: make(map[int]int)},
	"keySizeDist":       &keySizeDistribution{rsaSizes: make(map[int]int), dsaSizes: make(map[int]int), ellipticSizes: make(map[int]int)},
	"keyTypeDist":       &keyTypeDistribution{keyTypes: make(map[string]int)},
}

func StringToMetrics(metricsString string) ([]metricGenerator, error) {
	var metrics []metricGenerator
	for _, metricName := range strings.Split(metricsString, ",") {
		if generator, present := metricsLookup[metricName]; present {
			metrics = append(metrics, generator)
		} else if !present {
			return nil, fmt.Errorf("invalid metric name")
		}
	}
	if len(metrics) == 0 {
		return nil, fmt.Errorf("at least one metric is required to continue")
	}
	return metrics, nil
}

var cutoffLookup = map[string]*int{
	"popularSuffixes": &popularSuffixesCutoff,
	"leafIssuers":     &leafIssuanceCutoff,
}

func StringToCutoffs(cutoffs string) error {
	sections := strings.Split(cutoffs, ",")
	if len(sections) == 0 {
		return fmt.Errorf("At least one properly formatted cutoff must be specified")
	}
	for i, f := range sections {
		fields := strings.Split(f, ":")
		if len(fields) != 2 {
			return fmt.Errorf("Cutoff definition [%d] had invalid format", i+1)
		}
		if cutoff, present := cutoffLookup[fields[0]]; !present {
			return fmt.Errorf("Cutoff '%s' does not exist", fields[0])
		} else if present {
			value, err := strconv.Atoi(fields[1])
			if err != nil {
				return fmt.Errorf("Cutoff count for '%s' is not an int: %s", fields[0], err)
			}
			*cutoff = value
		}
	}
	return nil
}

type certSizeDistribution struct {
	sizes map[int]int
	mu    sync.Mutex
}

func (csd *certSizeDistribution) process(cert *x509.Certificate) {
	certSize := int(math.Ceil(float64(len(cert.Raw))/100)) * int(100)
	csd.mu.Lock()
	defer csd.mu.Unlock()
	csd.sizes[certSize]++
}

func (csd *certSizeDistribution) print() {
	dist := intDistribution{}
	sum := 0
	for k, v := range csd.sizes {
		dist = append(dist, intBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Certificate size distribution")
	dist.print("Size (bytes)", sum)
}

type validityDistribution struct {
	periods map[int]int
	mu      sync.Mutex
}

func (vd *validityDistribution) process(cert *x509.Certificate) {
	period := int((cert.NotAfter.Sub(cert.NotBefore)).Hours() / 24 / 30)

	vd.mu.Lock()
	defer vd.mu.Unlock()
	vd.periods[period]++
}

func (vd *validityDistribution) print() {
	dist := intDistribution{}
	sum := 0
	for k, v := range vd.periods {
		dist = append(dist, intBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Validity period distribution")
	dist.print("Validity period (months)", sum)
}

type sanSizeDistribution struct {
	sizes map[int]int
	mu    sync.Mutex
}

func (ssd *sanSizeDistribution) process(cert *x509.Certificate) {
	size := len(cert.DNSNames)
	ssd.mu.Lock()
	defer ssd.mu.Unlock()
	ssd.sizes[size]++
}

func (ssd *sanSizeDistribution) print() {
	dist := intDistribution{}
	sum := 0
	for k, v := range ssd.sizes {
		dist = append(dist, intBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# SAN num distribution")
	dist.print("Number of SANs", sum)
}

type serialLengthDistribution struct {
	lengths map[int]int
	mu      sync.Mutex
}

func (sld *serialLengthDistribution) process(cert *x509.Certificate) {
	sld.mu.Lock()
	defer sld.mu.Unlock()
	sld.lengths[len(cert.SerialNumber.Bytes())]++
}

func (sld *serialLengthDistribution) print() {
	dist := intDistribution{}
	sum := 0
	for k, v := range sld.lengths {
		dist = append(dist, intBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Serial number length distribution")
	dist.print("Length (bytes)", sum)
}

type numExtensionsDistribution struct {
	extensions map[int]int
	mu         sync.Mutex
}

func (ned *numExtensionsDistribution) process(cert *x509.Certificate) {
	ned.mu.Lock()
	defer ned.mu.Unlock()
	ned.extensions[len(cert.Extensions)]++
}

func (ned *numExtensionsDistribution) print() {
	dist := intDistribution{}
	sum := 0
	for k, v := range ned.extensions {
		dist = append(dist, intBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# TLS extension number distribution")
	dist.print("Num TLS extensions", sum)
}

var pkAlgToString = map[x509.PublicKeyAlgorithm]string{
	0: "Unknown",
	1: "RSA",
	2: "DSA",
	3: "ECDSA",
}

type pkAlgDistribution struct {
	algs map[string]int
	mu   sync.Mutex
}

func (pad *pkAlgDistribution) process(cert *x509.Certificate) {
	alg, ok := pkAlgToString[cert.PublicKeyAlgorithm]
	if !ok {
		return
	}
	pad.mu.Lock()
	defer pad.mu.Unlock()
	pad.algs[alg]++
}

func (pad *pkAlgDistribution) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range pad.algs {
		dist = append(dist, strBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Public key type distribution")
	dist.print("Type", sum)
}

var sigAlgToString = map[x509.SignatureAlgorithm]string{
	0:  "Unknown",
	1:  "MD2 With RSA",
	2:  "MD5 With RSA",
	3:  "SHA1 With RSA",
	4:  "SHA256 With RSA",
	5:  "SHA384 With RSA",
	6:  "SHA512 With RSA",
	7:  "DSA With SHA1",
	8:  "DSA With SHA256",
	9:  "ECDSA With SHA1",
	10: "ECDSA With SHA256",
	11: "ECDSA With SHA384",
	12: "ECDSA With SHA512",
}

type sigAlgDistribution struct {
	algs map[string]int
	mu   sync.Mutex
}

func (sad *sigAlgDistribution) process(cert *x509.Certificate) {
	alg, ok := sigAlgToString[cert.SignatureAlgorithm]
	if !ok {
		return
	}
	sad.mu.Lock()
	defer sad.mu.Unlock()
	sad.algs[alg]++
}

func (sad *sigAlgDistribution) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range sad.algs {
		dist = append(dist, strBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Signature type distribution")
	dist.print("Type", sum)
}

type popularSuffixes struct {
	suffixes map[string]int
	mu       sync.Mutex
}

func (ps *popularSuffixes) process(cert *x509.Certificate) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, n := range cert.DNSNames {
		suffix, err := publicsuffix.EffectiveTLDPlusOne(n)
		if err != nil || suffix == n {
			continue
		}
		ps.suffixes[suffix]++
	}
}

var popularSuffixesCutoff = 500

func (ps *popularSuffixes) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range ps.suffixes {
		if v > popularSuffixesCutoff {
			dist = append(dist, strBucket{count: v, value: k})
			sum += v
		}
	}
	sort.Sort(dist)

	fmt.Println("# Popular DNS name suffixes")
	dist.print("eTLD+1", sum)
}

type leafIssuanceDist struct {
	issuances map[string]int
	mu        sync.Mutex
}

func (lid *leafIssuanceDist) process(cert *x509.Certificate) {
	lid.mu.Lock()
	defer lid.mu.Unlock()
	lid.issuances[common.SubjectToString(cert.Issuer)]++
}

var leafIssuanceCutoff = 500

func (lid *leafIssuanceDist) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range lid.issuances {
		if v > leafIssuanceCutoff {
			dist = append(dist, strBucket{count: v, value: k})
			sum += v
		}
	}
	sort.Sort(dist)

	fmt.Println("# Leaf issuers")
	dist.print("Num issuances", sum)
}

var keyUsageLookup = map[x509.ExtKeyUsage]string{
	0:  "ExtKeyUsageAny",
	1:  "ExtKeyUsageServerAuth",
	2:  "ExtKeyUsageClientAuth",
	3:  "ExtKeyUsageCodeSigning",
	4:  "ExtKeyUsageEmailProtection",
	5:  "ExtKeyUsageIPSECEndSystem",
	6:  "ExtKeyUsageIPSECTunnel",
	7:  "ExtKeyUsageIPSECUser",
	8:  "ExtKeyUsageTimeStamping",
	9:  "ExtKeyUsageOCSPSigning",
	10: "ExtKeyUsageMicrosoftServerGatedCrypto",
	11: "ExtKeyUsageNetscapeServerGatedCrypto",
}

type keyUsageDist struct {
	usage map[string]int
	mu    sync.Mutex
}

func (kud *keyUsageDist) process(cert *x509.Certificate) {
	usages := []string{}
	for _, u := range cert.ExtKeyUsage {
		if name, present := keyUsageLookup[u]; present {
			usages = append(usages, name)
		}
	}
	sort.Strings(usages)
	kud.mu.Lock()
	defer kud.mu.Unlock()
	kud.usage[strings.Join(usages, ", ")]++
}

func (kud *keyUsageDist) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range kud.usage {
		dist = append(dist, strBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Key usage distribution")
	dist.print("Usages", sum)
}

type keyTypeDistribution struct {
	keyTypes map[string]int
	mu       sync.Mutex
}

func (ktd *keyTypeDistribution) process(cert *x509.Certificate) {
	ktd.mu.Lock()
	defer ktd.mu.Unlock()
	switch cert.PublicKey.(type) {
	case *rsa.PublicKey:
		ktd.keyTypes["RSA"]++
	case *dsa.PublicKey:
		ktd.keyTypes["DSA"]++
	case *ecdsa.PublicKey:
		ktd.keyTypes["ECDSA"]++
	}
}

func (ktd *keyTypeDistribution) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range ktd.keyTypes {
		dist = append(dist, strBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# Key type distribution")
	dist.print("Type", sum)
}

type nameMetrics struct {
	nMu        sync.Mutex
	names      map[string]int
	totalNames int64

	nsMu          sync.Mutex
	nameSets      map[string]int
	totalNameSets int64
}

func (nm *nameMetrics) process(cert *x509.Certificate) {
	atomic.AddInt64(&nm.totalNameSets, 1)
	atomic.AddInt64(&nm.totalNames, int64(len(cert.DNSNames)))
	sort.Strings(cert.DNSNames)
	nameSet := strings.Join(cert.DNSNames, ",")
	nm.nsMu.Lock()
	nm.nameSets[nameSet]++
	nm.nsMu.Unlock()
	for _, name := range cert.DNSNames {
		nm.nMu.Lock()
		nm.names[name]++
		nm.nMu.Unlock()
	}
}

func (nm *nameMetrics) print() {
	fmt.Printf("# DNS name metrics\n\n")
	fmt.Printf("%d names across %d certificates\n", nm.totalNames, nm.totalNameSets)
	fmt.Printf(
		"%.2f%% of names existed in multiple certificates\n%.2f%% of certificates had duplicate name sets\n",
		(1.0-(float64(len(nm.names))/float64(nm.totalNames)))*100.0,
		(1.0-(float64(len(nm.nameSets))/float64(nm.totalNameSets)))*100.0,
	)
}

func oidToString(oid asn1.ObjectIdentifier) string {
	if len(oid) == 0 {
		return ""
	}
	if len(oid) == 1 {
		return fmt.Sprintf("%d", oid[0])
	}
	n := (len(oid) - 1)
	for i := 0; i < len(oid); i++ {
		n += len(fmt.Sprintf("%d", oid[i]))
	}

	b := make([]byte, n)
	bp := copy(b, fmt.Sprintf("%d", oid[0]))
	for _, s := range oid[1:] {
		bp += copy(b[bp:], ".")
		bp += copy(b[bp:], fmt.Sprintf("%d", s))
	}
	return string(b)
}

var featureLookup = map[string]string{
	"1.3.6.1.4.1.11129.2.4.2": "Embedded SCT",
	"1.3.6.1.5.5.7.1.24":      "OCSP must staple",
}

type featureMetrics struct {
	features map[string]int
	mu       sync.Mutex
}

func (fm *featureMetrics) process(cert *x509.Certificate) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, e := range cert.Extensions {
		if name, present := featureLookup[oidToString(e.Id)]; present {
			fm.features[name]++
		}
	}
}

func (fm *featureMetrics) print() {
	dist := strDistribution{}
	sum := 0
	for k, v := range fm.features {
		dist = append(dist, strBucket{count: v, value: k})
		sum += v
	}
	sort.Sort(dist)

	fmt.Println("# TLS feature extension usage")
	dist.print("Extension name", sum)
}

type keySizeDistribution struct {
	rsaSizes      map[int]int
	rMu           sync.Mutex
	dsaSizes      map[int]int
	dMu           sync.Mutex
	ellipticSizes map[int]int
	eMu           sync.Mutex
}

func (ksd *keySizeDistribution) process(cert *x509.Certificate) {
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		ksd.rMu.Lock()
		defer ksd.rMu.Unlock()
		ksd.rsaSizes[k.N.BitLen()]++
	case *dsa.PublicKey:
		ksd.dMu.Lock()
		defer ksd.dMu.Unlock()
		ksd.dsaSizes[k.Y.BitLen()]++
	case *ecdsa.PublicKey:
		ksd.eMu.Lock()
		defer ksd.eMu.Unlock()
		ksd.ellipticSizes[k.Params().BitSize]++
	}
}

func (ksd *keySizeDistribution) print() {
	dsaDist := intDistribution{}
	dsaSum := 0
	for k, v := range ksd.dsaSizes {
		dsaDist = append(dsaDist, intBucket{count: v, value: k})
		dsaSum += v
	}
	sort.Sort(dsaDist)
	rsaDist := intDistribution{}
	rsaSum := 0
	for k, v := range ksd.rsaSizes {
		rsaDist = append(rsaDist, intBucket{count: v, value: k})
		rsaSum += v
	}
	sort.Sort(rsaDist)
	ecDist := intDistribution{}
	ecSum := 0
	for k, v := range ksd.ellipticSizes {
		ecDist = append(ecDist, intBucket{count: v, value: k})
		ecSum += v
	}
	sort.Sort(ecDist)

	fmt.Println("# DSA key size distribution")
	dsaDist.print("Bit length", dsaSum)
	fmt.Println("# RSA key size distribution")
	rsaDist.print("Bit length", rsaSum)
	fmt.Println("# ECDSA key size distribution")
	ecDist.print("Bit length", ecSum)
}

func Analyse(cacheFile string, filters []filter.Filter, generators []metricGenerator) error {
	entries, err := common.LoadCacheFile(cacheFile)
	if err != nil {
		return err
	}

	cMu := new(sync.Mutex)
	ctErrors := make(map[string]int)
	xMu := new(sync.Mutex)
	x509Errors := make(map[string]int)
	entries.Map(func(ent *ct.EntryAndPosition, err error) {
		if err != nil {
			cMu.Lock()
			ctErrors[err.Error()]++
			cMu.Unlock()
			return
		}
		// execute CT entry metric stuff (TODO)
		cert, skip, err := common.ParseAndFilter(ent.Entry.X509Cert, filters)
		if !skip && err != nil {
			xMu.Lock()
			x509Errors[err.Error()]++
			xMu.Unlock()
			return
		} else if err == nil && skip {
			return
		}
		// execute leaf metric generators
		wg := new(sync.WaitGroup)
		for _, g := range generators {
			wg.Add(1)
			go func(mg metricGenerator) {
				mg.process(cert)
				wg.Done()
			}(g)
		}
		wg.Wait()
	})

	for _, g := range generators {
		g.print()
		fmt.Println("")
	}

	return nil
}
