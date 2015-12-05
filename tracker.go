package main

import (
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ct "github.com/jsha/certificatetransparency"
)

type testEntry struct {
	leaf *x509.Certificate
}

type tester struct {
	totalCerts     int
	processedCerts int64
	totalNames     int
	processedNames int64

	namesUnavailable   int64
	namesHTTPSDisabled int64
	namesCertNotUsed   int64

	certsUnused int64

	workers int

	entries chan *testEntry

	client *http.Client

	expectedChainLength int
}

type result struct {
	hostAvailable bool
	httpsEnabled  bool
	certUsed      bool
	properlySetup bool
}

func (t *tester) processResults(results []result) {
	unused := true
	for _, r := range results {
		if !r.hostAvailable {
			atomic.AddInt64(&t.namesUnavailable, 1)
			continue
		}
		if !r.httpsEnabled {
			atomic.AddInt64(&t.namesHTTPSDisabled, 1)
			continue
		}
		if !r.certUsed {
			atomic.AddInt64(&t.namesCertNotUsed, 1)
			continue
		}
		if unused {
			unused = false
		}
	}
	if unused {
		atomic.AddInt64(&t.certsUnused, 1)
	}
}

func (t *tester) printProgress(stop chan bool) {
	prog := ""
	for {
		select {
		case <-stop:
			return
		default:
			processedCerts := atomic.LoadInt64(&t.processedCerts)
			processedNames := atomic.LoadInt64(&t.processedNames)
			namesUnavailable := atomic.LoadInt64(&t.namesUnavailable)
			namesHTTPSDisabled := atomic.LoadInt64(&t.namesHTTPSDisabled)
			namesCertsNotUsed := atomic.LoadInt64(&t.namesCertNotUsed)
			certsUnused := atomic.LoadInt64(&t.certsUnused)

			if prog != "" {
				fmt.Fprintf(os.Stdout, strings.Repeat("\b", len(prog)))
			}
			prog = fmt.Sprintf(
				"%d/%d certificates checked (%d/%d names) names unavailable: %d, names redirected to http: %d, names not using expected cert: %d, unused certificates: %d",
				processedCerts,
				t.totalCerts,
				processedNames,
				t.totalNames,
				namesUnavailable,
				namesHTTPSDisabled,
				namesCertsNotUsed,
				certsUnused,
			)
			fmt.Fprintf(os.Stdout, prog)
			time.Sleep(time.Second)
		}
	}
}

func (t *tester) checkName(dnsName string, expectedFP [32]byte) (r result) {
	defer atomic.AddInt64(&t.processedNames, 1)
	resp, err := t.client.Get(fmt.Sprintf("https://%s", dnsName))
	if err != nil {
		// this should probably retry on some set of errors :/
		return
	}
	defer resp.Body.Close()
	r.hostAvailable = true
	if resp.TLS == nil {
		return
	}
	r.httpsEnabled = true
	for _, peer := range resp.TLS.PeerCertificates {
		if sha256.Sum256(peer.Raw) != expectedFP {
			r.certUsed = true
		}
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
	go t.processResults(results)
}

func (t *tester) begin() {
	fmt.Println("beginning adoption scan")
	stop := make(chan bool, 1)
	go t.printProgress(stop)
	wg := new(sync.WaitGroup)
	for i := 0; i < t.workers; i++ {
		wg.Add(1)
		go func() {
			for te := range t.entries {
				t.checkCert(te.leaf)
			}
			wg.Done()
		}()
	}
	wg.Wait()
	stop <- true
}

func loadAndUpdate(logURL, logKey, filename, issuerFilter string) (chan *testEntry, int) {
	pemPublicKey := fmt.Sprintf(`-----BEGIN PUBLIC KEY-----
%s
-----END PUBLIC KEY-----`, logKey)
	ctLog, err := ct.NewLog(logURL, pemPublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize log: %s\n", err)
		os.Exit(1)
	}

	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open entries file: %s\n", err)
		os.Exit(1)
	}
	defer file.Close()

	entriesFile := ct.EntriesFile{file}

	sth, err := ctLog.GetSignedTreeHead()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetSignedTreeHead: %s\n", err)
		os.Exit(1)
	}

	count, err := entriesFile.Count()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed to read entries file: %s\n", err)
		os.Exit(1)
	}
	fmt.Printf("local entries: %d, remote entries: %d at %s\n", count, sth.Size, sth.Time.Format(time.ANSIC))
	if count < sth.Size {
		fmt.Println("updating local cache...")
		_, err = ctLog.DownloadRange(file, nil, count, sth.Size)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nFailed to update CT log: %s\n", err)
			os.Exit(1)
		}
	}
	entriesFile.Seek(0, 0)

	fmt.Printf("filtering local cache for certificates with issuer '%s'\n", issuerFilter)
	filtered := make(chan *testEntry, sth.Size)
	numNames := 0
	entriesFile.Map(func(ent *ct.EntryAndPosition, err error) {
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
		numNames += len(cert.DNSNames)
		filtered <- &testEntry{leaf: cert}
	})
	close(filtered)
	return filtered, numNames
}

func main() {
	entries, numNames := loadAndUpdate("https://log.certly.io", "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAECyPLhWKYYUgEc+tUXfPQB4wtGS2MNvXrjwFCCnyYJifBtd2Sk7Cu+Js9DNhMTh35FftHaHu6ZrclnNBKwmbbSA==", "certly.log", "Let's Encrypt Authority X1")
	t := tester{
		entries:    entries,
		totalCerts: len(entries),
		totalNames: numNames,
		client:     new(http.Client),
		workers:    50,
	}
	t.begin()
}
