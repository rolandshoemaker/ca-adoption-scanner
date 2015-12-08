package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ct "github.com/jsha/certificatetransparency"
)

type collectedResults struct {
	NamesSkipped              int64
	NamesUnavailable          int64
	NamesTLSError             int64
	NamesUsingMiscInvalidCert int64
	NamesUsingExpiredCert     int64
	NamesUsingIncompleteChain int64
	NamesUsingWrongCert       int64
	NamesUsingSelfSignedCert  int64
	NamesWithOCSPStapled      int64
	NamesServingSCTs          int64
	NamesCertNotUsed          int64

	CertsUnused        int64
	CertsPartiallyUsed int64
	CertsTotallyUsed   int64
}

type tester struct {
	// progress stuff
	totalCerts        int
	processedCerts    int64
	totalNames        int64
	processedNames    int64
	progPrintInterval float64
	dontPrintProgress bool

	// important stuff
	results       collectedResults
	workers       int
	entries       chan *x509.Certificate
	dialerTimeout time.Duration

	// misc
	debug bool
}

type result struct {
	skipped              bool
	hostAvailable        bool
	tlsError             bool
	usingMiscInvalidCert bool
	usingExpiredCert     bool
	usingIncompleteChain bool
	usingWrongCert       bool
	usingSelfSignedCert  bool
	certUsed             bool
	ocspStapled          bool
	servesSCTs           bool
}

func (t *tester) processResults(results []result) {
	used := 0
	for _, r := range results {
		if r.skipped {

			atomic.AddInt64(&t.results.NamesSkipped, 1)
			continue
		}
		if !r.hostAvailable {
			atomic.AddInt64(&t.results.NamesUnavailable, 1)
			continue
		}
		if r.tlsError {
			atomic.AddInt64(&t.results.NamesTLSError, 1)
			continue
		}
		if r.usingMiscInvalidCert {
			atomic.AddInt64(&t.results.NamesUsingMiscInvalidCert, 1)
			continue
		} else if r.usingExpiredCert {
			atomic.AddInt64(&t.results.NamesUsingExpiredCert, 1)
			continue
		} else if r.usingIncompleteChain {
			atomic.AddInt64(&t.results.NamesUsingIncompleteChain, 1)
			continue
		} else if r.usingWrongCert {
			atomic.AddInt64(&t.results.NamesUsingWrongCert, 1)
			continue
		} else if r.usingSelfSignedCert {
			atomic.AddInt64(&t.results.NamesUsingSelfSignedCert, 1)
			continue
		}
		if !r.certUsed {
			atomic.AddInt64(&t.results.NamesCertNotUsed, 1)
			continue
		}
		if r.ocspStapled {
			atomic.AddInt64(&t.results.NamesWithOCSPStapled, 1)
		}
		if r.servesSCTs {
			atomic.AddInt64(&t.results.NamesServingSCTs, 1)
		}
		used++
	}
	if used == len(results) {
		atomic.AddInt64(&t.results.CertsTotallyUsed, 1)
	} else if used < len(results) && used > 0 {
		atomic.AddInt64(&t.results.CertsPartiallyUsed, 1)
	} else if used == 0 {
		atomic.AddInt64(&t.results.CertsUnused, 1)
	}
}

func (t *tester) printProgress(stop chan bool) {
	prog := ""
	started := time.Now()
	for {
		select {
		case <-stop:
			return
		default:
			processedCerts := atomic.LoadInt64(&t.processedCerts)
			processedNames := atomic.LoadInt64(&t.processedNames)
			taken := time.Since(started).Seconds()
			cps := float64(processedCerts) / taken
			nps := float64(processedNames) / taken
			eta := "???"
			etaDur := time.Second * time.Duration(float64(t.totalNames-processedNames)/nps)
			if etaDur > time.Second && etaDur < (24*time.Hour) {
				eta = etaDur.String()
			}
			if prog != "" {
				// Assume VT100 because \b is terrible
				fmt.Fprintf(os.Stdout, fmt.Sprintf("\033[1K\033[%dD", len(prog)))
			}
			prog = fmt.Sprintf(
				"%d/%d certificates checked, %d/%d names [%.2f cps, %.2f nps, eta: %s]",
				processedCerts,
				t.totalCerts,
				processedNames,
				t.totalNames,
				cps,
				nps,
				eta,
			)
			fmt.Fprintf(os.Stdout, prog)
			time.Sleep(time.Second * time.Duration(t.progPrintInterval))
		}
	}
}

func percent(n, t int64) float64 {
	return (float64(n) / float64(t)) * 100.0
}

func (t *tester) printStats() {
	fmt.Println("\n# adoption statistics")
	fmt.Printf("%d certificates checked (totalling %d DNS names)\n", t.processedCerts, t.processedNames)
	fmt.Println()
	fmt.Printf("%d (%.2f%%) names skipped\n", t.results.NamesSkipped, percent(t.results.NamesSkipped, t.processedNames))
	fmt.Printf("%d (%.2f%%) names couldn't be connected to\n", t.results.NamesUnavailable, percent(t.results.NamesUnavailable, t.processedNames))
	fmt.Printf("%d (%.2f%%) names threw a TLS handshake error\n", t.results.NamesTLSError, percent(t.results.NamesTLSError, t.processedNames))
	fmt.Printf("%d (%.2f%%) names sent a incomplete chain\n", t.results.NamesUsingIncompleteChain, percent(t.results.NamesUsingIncompleteChain, t.processedNames))
	fmt.Printf("%d (%.2f%%) names used a expired certificate\n", t.results.NamesUsingExpiredCert, percent(t.results.NamesUsingExpiredCert, t.processedNames))
	fmt.Printf("%d (%.2f%%) names used a self signed certificate\n", t.results.NamesUsingSelfSignedCert, percent(t.results.NamesUsingSelfSignedCert, t.processedNames))
	fmt.Printf("%d (%.2f%%) names used a certificate for names that didn't match\n", t.results.NamesUsingWrongCert, percent(t.results.NamesUsingWrongCert, t.processedNames))
	fmt.Printf("%d (%.2f%%) names used a invalid certificate (misc. reasons)\n", t.results.NamesUsingMiscInvalidCert, percent(t.results.NamesUsingMiscInvalidCert, t.processedNames))
	fmt.Printf("%d (%.2f%%) names didn't use their certificate\n", t.results.NamesCertNotUsed, percent(t.results.NamesCertNotUsed, t.processedNames))
	fmt.Println()
	fmt.Printf("%d (%.2f%%) names had a stapled OCSP response\n", t.results.NamesWithOCSPStapled, percent(t.results.NamesWithOCSPStapled, t.processedNames))
	fmt.Printf("%d (%.2f%%) names served SCT receipts\n", t.results.NamesServingSCTs, percent(t.results.NamesServingSCTs, t.processedNames))
	fmt.Println()
	fmt.Printf("%d (%.2f%%) certificates were used by none of their names\n", t.results.CertsUnused, percent(t.results.CertsUnused, int64(t.processedCerts)))
	fmt.Printf("%d (%.2f%%) certificates were used by some of their names\n", t.results.CertsPartiallyUsed, percent(t.results.CertsPartiallyUsed, int64(t.processedCerts)))
	fmt.Printf("%d (%.2f%%) certificates were used by all of their names\n", t.results.CertsTotallyUsed, percent(t.results.CertsTotallyUsed, int64(t.processedCerts)))
}

func (t *tester) checkName(dnsName string, expectedFP [32]byte) (r result) {
	defer atomic.AddInt64(&t.processedNames, 1)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: t.dialerTimeout}, "tcp", fmt.Sprintf("%s:443", dnsName), nil)
	if err != nil {
		// this should probably retry on some set of errors :/
		if t.debug {
			fmt.Printf("Connection to [%s] failed: %s\n", dnsName, err)
		}
		// Check if the error failed because connection was refused / timed out / DNS broke
		if netErr, ok := err.(*net.OpError); ok {
			if netErr.Timeout() || netErr.Temporary() {
				r.skipped = true
			}
			if dnsErr, ok := netErr.Err.(*net.DNSError); ok {
				if dnsErr.Timeout() || dnsErr.Temporary() {
					r.skipped = true
				}
			}
			// Hosts that don't serve HTTPS are marked unavailable (this should be noted elsewhere...)
			return
		}
		r.hostAvailable = true
		// Check if the error was TLS related
		if strings.HasPrefix(err.Error(), "tls:") || err.Error() == "EOF" {
			r.tlsError = true
			return
		}
		if _, ok := err.(x509.UnknownAuthorityError); ok {
			r.usingIncompleteChain = true
			return
		}
		if _, ok := err.(x509.HostnameError); ok {
			r.usingWrongCert = true
			return
		}
		if invErr, ok := err.(x509.CertificateInvalidError); ok {
			if invErr.Reason == x509.Expired {
				r.usingExpiredCert = true
				return
			} else if invErr.Reason == x509.NotAuthorizedToSign {
				r.usingSelfSignedCert = true
				return
			}
		}
		r.usingMiscInvalidCert = true
		return
	}
	r.hostAvailable = true
	defer conn.Close()
	state := conn.ConnectionState()
	if !state.HandshakeComplete {
		return
	}
	for _, peer := range state.PeerCertificates {
		if sha256.Sum256(peer.Raw) == expectedFP {
			r.certUsed = true
			break
		}
	}
	if len(state.OCSPResponse) != 0 {
		r.ocspStapled = true
	}
	if len(state.SignedCertificateTimestamps) != 0 {
		r.servesSCTs = true
	}
	return
}

func (t *tester) checkCert(cert *x509.Certificate) {
	defer atomic.AddInt64(&t.processedCerts, 1)
	fp := sha256.Sum256(cert.Raw)
	var results []result
	for _, name := range cert.DNSNames {
		results = append(results, t.checkName(name, fp))
	}
	t.processResults(results)
}

func (t *tester) begin() {
	fmt.Printf("beginning adoption scan of %d certificates (%d names)\n", t.totalCerts, t.totalNames)
	stopProg := make(chan bool, 1)
	if !t.debug && !t.dontPrintProgress {
		go t.printProgress(stopProg)
	}
	wg := new(sync.WaitGroup)
	started := time.Now()
	stopWorkers := []chan bool{}
	for i := 0; i < t.workers; i++ {
		stop := make(chan bool, 1)
		stopWorkers = append(stopWorkers, stop)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for te := range t.entries {
				select {
				case <-stop:
					return
				default:
					t.checkCert(te)
				}
			}
		}()
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		<-sigChan
		stopProg <- true
		fmt.Println("\n\ninterrupted, cleaning up")
		for _, sw := range stopWorkers {
			sw <- true
		}
	}()
	wg.Wait()
	stopProg <- true
	fmt.Printf("\n\nscan finished, took %s\n", time.Since(started))
}

func (t *tester) filterOnIssuer(issuerFilter string) func(*ct.EntryAndPosition, error) {
	return func(ent *ct.EntryAndPosition, err error) {
		if err != nil {
			return
		}
		cert, err := x509.ParseCertificate(ent.Entry.X509Cert)
		if err != nil {
			return
		}
		if cert.Issuer.CommonName != issuerFilter {
			return
		}
		if time.Now().After(cert.NotAfter) {
			return
		}
		atomic.AddInt64(&t.totalNames, int64(len(cert.DNSNames)))
		t.entries <- cert
	}
}

func (t *tester) filterOnIssuerAndSample(issuerFilter string) func(*ct.EntryAndPosition, error) {
	return nil
}

func (t *tester) loadAndUpdate(logURL, logKey, filename string, dontUpdate bool, filterFunc func(*ct.EntryAndPosition, error)) error {
	pemPublicKey := fmt.Sprintf(`-----BEGIN PUBLIC KEY-----
%s
-----END PUBLIC KEY-----`, logKey)
	ctLog, err := ct.NewLog(logURL, pemPublicKey)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer file.Close()

	entriesFile := ct.EntriesFile{file}

	sth, err := ctLog.GetSignedTreeHead()
	if err != nil {
		return err
	}

	count, err := entriesFile.Count()
	if err != nil {
		return err
	}
	fmt.Printf("local entries: %d, remote entries: %d at %s\n", count, sth.Size, sth.Time.Format(time.ANSIC))
	if !dontUpdate && count < sth.Size {
		fmt.Println("updating local cache...")
		_, err = ctLog.DownloadRange(file, nil, count, sth.Size)
		if err != nil {
			return err
		}
	}
	entriesFile.Seek(0, 0)

	fmt.Println("filtering local cache")
	t.entries = make(chan *x509.Certificate, sth.Size)
	entriesFile.Map(filterFunc)
	if len(t.entries) == 0 {
		return fmt.Errorf("filtered list contains no certificates!")
	}
	close(t.entries)
	t.totalCerts = len(t.entries)
	return nil
}

func main() {
	logURL := flag.String("logURL", "https://log.certly.io", "url of remote CT log to use")
	logKey := flag.String("logKey", "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAECyPLhWKYYUgEc+tUXfPQB4wtGS2MNvXrjwFCCnyYJifBtd2Sk7Cu+Js9DNhMTh35FftHaHu6ZrclnNBKwmbbSA==", "base64-encoded CT log key")
	filename := flag.String("cacheFile", "certly.log", "file in which to cache log data.")
	issuerFilter := flag.String("issuerFilter", "Let's Encrypt Authority X1", "common name of issuer to use as a filter")
	scanners := flag.Int("scanners", 50, "number of scanner workers to run")
	dontUpdateCache := flag.Bool("dontUpdateCache", false, "don't update the local log cache")
	debug := flag.Bool("debug", false, "print lots of error messages")
	dontPrintProgress := flag.Bool("dontPrintProgress", false, "don't print progress information")
	scannerTimeout := flag.Duration("scannerTimeout", time.Second*5, "dialer timeout for the tls scanners (uses golang duration format, e.g. 5s)")
	flag.Parse()

	t := tester{
		workers:           *scanners,
		progPrintInterval: 5.0,
		debug:             *debug,
		dontPrintProgress: *dontPrintProgress,
		dialerTimeout:     *scannerTimeout,
	}

	err := t.loadAndUpdate(*logURL, *logKey, *filename, *dontUpdateCache, t.filterOnIssuer(*issuerFilter))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load, update, and filter the local CT cache file: %s\n", err)
		os.Exit(1)
	}

	t.begin()
	t.printStats()
}
