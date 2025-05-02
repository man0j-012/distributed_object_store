// cmd/server/main.go
// Author: Manoj Myneni – AVID-FP Object Store (v2.3.2, Apr 2025)
// Prometheus metrics, -metricsPort flag, and automatic self-echo so every
// node reaches quorum even if its own address is omitted from -peers.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dattu/distributed_object_store/pkg/fingerprint"
	"github.com/dattu/distributed_object_store/pkg/protocol"
	"github.com/dattu/distributed_object_store/pkg/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
)

/* ------------------------------------------------------------------------ */
/* constants                                                                */
/* ------------------------------------------------------------------------ */

const (
	defaultPort      = 50051
	disperseTimeout  = 20 * time.Second
	echoDialTimeout  = 5 * time.Second
	readyDialTimeout = 5 * time.Second

	fpccsBucket = "fpccs"
	echoBucket  = "echoSeen"
	readyBucket = "readySeen"
	metaBucket  = "meta"
)

/* ------------------------------------------------------------------------ */
/* Prometheus metrics                                                       */
/* ------------------------------------------------------------------------ */

var (
	disperseTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "avid_fp_disperse_total",
		Help: "Total Disperse RPC calls.",
	})
	disperseLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "avid_fp_disperse_duration_seconds",
		Help:    "Latency of Disperse RPCs.",
		Buckets: prometheus.DefBuckets,
	})
	retrieveTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "avid_fp_retrieve_total",
		Help: "Total Retrieve RPC calls.",
	})
	retrieveLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "avid_fp_retrieve_duration_seconds",
		Help:    "Latency of Retrieve RPCs.",
		Buckets: prometheus.DefBuckets,
	})
)

/* ------------------------------------------------------------------------ */
/* gRPC dial helper                                                         */
/* ------------------------------------------------------------------------ */

func dialOpts() grpc.DialOption {
	return grpc.WithTransportCredentials(insecure.NewCredentials())
}

/* ------------------------------------------------------------------------ */
/* server struct                                                            */
/* ------------------------------------------------------------------------ */

type server struct {
	protocol.UnimplementedDispersalServer

	peers               []string
	selfAddr            string // host:port string for this node
	m, n, f             int
	metaDB              *bolt.DB
	dataDir             string
	ttl                 time.Duration
	echoBatcher         *storage.Batcher
	readyBatcher        *storage.Batcher
	mu                  sync.Mutex
	fpccs               map[string]*protocol.FPCC
	echoSeen, readySeen map[string]map[string]bool
	readySent           map[string]bool
	commitChan          map[string]chan struct{}
}

/* ------------------------------------------------------------------------ */
/* constructor                                                              */
/* ------------------------------------------------------------------------ */

func newServer(self string, peers []string, m, n int, db *bolt.DB, dataDir string, ttl time.Duration) *server {
	echo := make(map[string]map[string]bool)
	ready := make(map[string]map[string]bool)

	// reload persisted Echo / Ready
	_ = db.View(func(tx *bolt.Tx) error {
		for _, bkt := range []string{echoBucket, readyBucket} {
			b := tx.Bucket([]byte(bkt))
			b.ForEach(func(k, _ []byte) error {
				parts := strings.SplitN(string(k), "|", 2)
				if len(parts) == 2 {
					obj, peer := parts[0], parts[1]
					dest := echo
					if bkt == readyBucket {
						dest = ready
					}
					if dest[obj] == nil {
						dest[obj] = make(map[string]bool)
					}
					dest[obj][peer] = true
				}
				return nil
			})
		}
		return nil
	})

	return &server{
		selfAddr:     self,
		peers:        peers,
		m:            m,
		n:            n,
		f:            n - m,
		metaDB:       db,
		dataDir:      dataDir,
		ttl:          ttl,
		fpccs:        make(map[string]*protocol.FPCC),
		echoSeen:     echo,
		readySeen:    ready,
		readySent:    make(map[string]bool),
		commitChan:   make(map[string]chan struct{}),
		echoBatcher:  storage.NewBatcher(db, echoBucket),
		readyBatcher: storage.NewBatcher(db, readyBucket),
	}
}

/* ------------------------------------------------------------------------ */
/* helpers                                                                  */
/* ------------------------------------------------------------------------ */

func (s *server) fragPath(obj string, idx uint32) string {
	return filepath.Join(s.dataDir, obj, fmt.Sprintf("%d.bin", idx))
}

func (s *server) persistFragment(obj string, idx uint32, data []byte) error {
	path := s.fragPath(obj, idx)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return storage.AtomicWrite(path, data, 0o644)
}

func (s *server) loadFragment(obj string, idx uint32) ([]byte, error) {
	return os.ReadFile(s.fragPath(obj, idx))
}

func eqFPCC(a, b *protocol.FPCC) bool {
	if a.Seed != b.Seed || len(a.Hashes) != len(b.Hashes) || len(a.Fps) != len(b.Fps) {
		return false
	}
	for i := range a.Hashes {
		if !bytes.Equal(a.Hashes[i], b.Hashes[i]) || a.Fps[i] != b.Fps[i] {
			return false
		}
	}
	return true
}

/* ------------------------------------------------------------------------ */
/* network broadcasters                                                     */
/* ------------------------------------------------------------------------ */

func (s *server) broadcastEcho(objectID string, fpcc *protocol.FPCC) {
	for _, addr := range s.peers {
		go func(a string) {
			ctx, cancel := context.WithTimeout(context.Background(), echoDialTimeout)
			conn, err := grpc.DialContext(ctx, a, dialOpts(), grpc.WithBlock())
			cancel()
			if err == nil {
				defer conn.Close()
				protocol.NewDispersalClient(conn).Echo(context.Background(),
					&protocol.EchoRequest{ObjectId: objectID, Fpcc: fpcc})
			}
		}(addr)
	}
}

func (s *server) broadcastReady(objectID string, fpcc *protocol.FPCC) {
	for _, addr := range s.peers {
		go func(a string) {
			ctx, cancel := context.WithTimeout(context.Background(), readyDialTimeout)
			conn, err := grpc.DialContext(ctx, a, dialOpts(), grpc.WithBlock())
			cancel()
			if err == nil {
				defer conn.Close()
				protocol.NewDispersalClient(conn).Ready(context.Background(),
					&protocol.ReadyRequest{ObjectId: objectID, Fpcc: fpcc})
			}
		}(addr)
	}
}

/* ------------------------------------------------------------------------ */
/* RPC – Disperse                                                           */
/* ------------------------------------------------------------------------ */

func (s *server) Disperse(ctx context.Context, req *protocol.DisperseRequest) (*protocol.DisperseResponse, error) {
	timer := prometheus.NewTimer(disperseLatency)
	defer timer.ObserveDuration()
	disperseTotal.Inc()

	log.Printf("[Disperse] %s idx=%d bytes=%d", req.ObjectId, req.FragmentIndex, len(req.Fragment))

	/* ... commit-channel & self-echo setup ... */
	s.mu.Lock()
	if _, ok := s.commitChan[req.ObjectId]; !ok {
		s.commitChan[req.ObjectId] = make(chan struct{})
		s.fpccs[req.ObjectId] = req.Fpcc

		// count our own Echo immediately
		if s.echoSeen[req.ObjectId] == nil {
			s.echoSeen[req.ObjectId] = make(map[string]bool)
		}
		s.echoSeen[req.ObjectId][s.selfAddr] = true

		s.metaDB.Update(func(tx *bolt.Tx) error {
			raw, _ := json.Marshal(struct{ Created time.Time }{time.Now()})
			return tx.Bucket([]byte(metaBucket)).Put([]byte(req.ObjectId), raw)
		})
	} else if !eqFPCC(s.fpccs[req.ObjectId], req.Fpcc) {
		s.mu.Unlock()
		return &protocol.DisperseResponse{Ok: false, Error: "FPCC mismatch"}, nil
	}
	commitCh := s.commitChan[req.ObjectId]
	s.mu.Unlock()

	/* integrity checks */
	if h := sha256.Sum256(req.Fragment); !bytes.Equal(h[:], req.Fpcc.Hashes[req.FragmentIndex]) {
		return &protocol.DisperseResponse{Ok: false, Error: "hash mismatch"}, nil
	}
	if fingerprint.NewWithSeed(req.Fpcc.Seed).Eval(req.Fragment) != req.Fpcc.Fps[req.FragmentIndex] {
		return &protocol.DisperseResponse{Ok: false, Error: "fingerprint mismatch"}, nil
	}

	/* persist fragment & FPCC */
	if err := s.persistFragment(req.ObjectId, req.FragmentIndex, req.Fragment); err != nil {
		return &protocol.DisperseResponse{Ok: false, Error: "fragment write"}, nil
	}
	_ = s.metaDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(fpccsBucket))
		if b.Get([]byte(req.ObjectId)) == nil {
			raw, _ := json.Marshal(req.Fpcc)
			b.Put([]byte(req.ObjectId), raw)
		}
		return nil
	})

	/* gossip Echo & await quorum */
	go s.broadcastEcho(req.ObjectId, req.Fpcc)

	select {
	case <-commitCh:
		return &protocol.DisperseResponse{Ok: true}, nil
	case <-time.After(disperseTimeout):
		return &protocol.DisperseResponse{Ok: false, Error: "timeout waiting for readies"}, nil
	}
}

/* ------------------------------------------------------------------------ */
/* RPC – Echo, Ready, Retrieve (unchanged from v2.3.1)                      */
/* ------------------------------------------------------------------------ */
/* --- Echo --- */

func (s *server) Echo(ctx context.Context, req *protocol.EchoRequest) (*protocol.EchoResponse, error) {
	peerAddr := func() string {
		if p, ok := peer.FromContext(ctx); ok {
			return p.Addr.String()
		}
		return ""
	}()
	s.mu.Lock()
	if s.echoSeen[req.ObjectId] == nil {
		s.echoSeen[req.ObjectId] = make(map[string]bool)
	}
	s.echoSeen[req.ObjectId][peerAddr] = true
	if len(s.echoSeen[req.ObjectId]) >= s.m+s.f && !s.readySent[req.ObjectId] {
		s.readySent[req.ObjectId] = true
		go s.broadcastReady(req.ObjectId, req.Fpcc)
	}
	s.mu.Unlock()

	s.echoBatcher.Put([]byte(fmt.Sprintf("%s|%s", req.ObjectId, peerAddr)), []byte{1})
	return &protocol.EchoResponse{Ok: true}, nil
}

/* --- Ready --- */

func (s *server) Ready(ctx context.Context, req *protocol.ReadyRequest) (*protocol.ReadyResponse, error) {
	peerAddr := func() string {
		if p, ok := peer.FromContext(ctx); ok {
			return p.Addr.String()
		}
		return ""
	}()
	s.mu.Lock()
	if s.readySeen[req.ObjectId] == nil {
		s.readySeen[req.ObjectId] = make(map[string]bool)
	}
	s.readySeen[req.ObjectId][peerAddr] = true
	if len(s.readySeen[req.ObjectId]) >= 2*s.f+1 {
		if ch := s.commitChan[req.ObjectId]; ch != nil {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}
	s.mu.Unlock()

	s.readyBatcher.Put([]byte(fmt.Sprintf("%s|%s", req.ObjectId, peerAddr)), []byte{1})
	return &protocol.ReadyResponse{Ok: true}, nil
}

/* --- Retrieve --- */

func (s *server) Retrieve(ctx context.Context, req *protocol.RetrieveRequest) (*protocol.RetrieveResponse, error) {
	timer := prometheus.NewTimer(retrieveLatency)
	defer timer.ObserveDuration()
	retrieveTotal.Inc()

	frag, err := s.loadFragment(req.ObjectId, req.FragmentIndex)
	if err != nil {
		return &protocol.RetrieveResponse{Ok: false, Error: "fragment missing"}, nil
	}
	s.mu.Lock()
	fpcc := s.fpccs[req.ObjectId]
	s.mu.Unlock()
	return &protocol.RetrieveResponse{
		Ok:            true,
		Fragment:      frag,
		FragmentIndex: req.FragmentIndex,
		Fpcc:          fpcc,
	}, nil
}

/* ------------------------------------------------------------------------ */
/* main                                                                     */
/* ------------------------------------------------------------------------ */

func main() {
	// register metrics
	prometheus.MustRegister(disperseTotal, disperseLatency, retrieveTotal, retrieveLatency)

	/* flags */
	port := flag.Int("port", defaultPort, "gRPC port")
	m := flag.Int("m", 3, "data shards")
	n := flag.Int("n", 5, "total shards")
	peersFlag := flag.String("peers", "", "comma-separated peers (optional)")
	metricsPort := flag.String("metricsPort", "9102", "HTTP port for /metrics")
	dbPath := flag.String("db", "", "BoltDB file")
	dataDir := flag.String("datadir", "data", "fragment directory")
	ttl := flag.Duration("ttl", 24*time.Hour, "object TTL")
	snapshot := flag.String("snapshot", "", "take snapshot & exit")
	flag.Parse()

	self := fmt.Sprintf("localhost:%d", *port)

	/* /metrics endpoint */
	go func() {
		addr := ":" + *metricsPort
		http.Handle("/metrics", promhttp.Handler())
		log.Printf("Prometheus metrics on %s/metrics", addr)
		log.Fatal(http.ListenAndServe(addr, nil))
	}()

	/* peer list (add self if missing) */
	peers := []string{}
	if *peersFlag != "" {
		peers = strings.FieldsFunc(*peersFlag, func(r rune) bool { return r == ',' })
	}
	found := false
	for _, p := range peers {
		if p == self {
			found = true
			break
		}
	}
	if !found {
		peers = append(peers, self)
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("mkdir datadir: %v", err)
	}
	if *dbPath == "" {
		*dbPath = fmt.Sprintf("store-%d.db", *port)
	}
	db, err := bolt.Open(*dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Fatalf("bolt.Open: %v", err)
	}
	defer db.Close()
	db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{fpccsBucket, echoBucket, readyBucket, metaBucket} {
			tx.CreateBucketIfNotExists([]byte(b))
		}
		return nil
	})

	if *snapshot != "" {
		runSnapshot(*dataDir, *dbPath, *snapshot)
		return
	}

	s := newServer(self, peers, *m, *n, db, *dataDir, *ttl)
	go s.gcLoop()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	protocol.RegisterDispersalServer(grpcServer, s)
	log.Printf("node %s  m=%d n=%d f=%d data=%s peers=%v metrics=%s",
		self, *m, *n, s.f, *dataDir, peers, *metricsPort)
	grpcServer.Serve(lis)
}

/* ------------------------------------------------------------------------ */
/* GC, snapshot helpers (unchanged)                                         */
/* ------------------------------------------------------------------------ */

func (s *server) gcLoop() {
	tick := time.NewTicker(s.ttl / 2)
	for range tick.C {
		s.gcExpired()
	}
}

func (s *server) gcExpired() {
	now := time.Now()
	var expired []string
	s.metaDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(metaBucket))
		b.ForEach(func(k, v []byte) error {
			var meta struct{ Created time.Time }
			if json.Unmarshal(v, &meta) == nil && now.Sub(meta.Created) > s.ttl {
				expired = append(expired, string(k))
			}
			return nil
		})
		return nil
	})
	for _, obj := range expired {
		s.deleteObject(obj)
	}
}

func (s *server) deleteObject(obj string) {
	log.Printf("GC delete %s", obj)
	os.RemoveAll(filepath.Join(s.dataDir, obj))
	s.metaDB.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{fpccsBucket, echoBucket, readyBucket, metaBucket} {
			bkt := tx.Bucket([]byte(b))
			if b == fpccsBucket || b == metaBucket {
				bkt.Delete([]byte(obj))
				continue
			}
			prefix := obj + "|"
			c := bkt.Cursor()
			for k, _ := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, _ = c.Next() {
				bkt.Delete(k)
			}
		}
		return nil
	})
}

func runSnapshot(dataDir, dbPath, dstDir string) {
	tag := time.Now().Format("20060102-150405")
	dst := filepath.Join(dstDir, tag)
	os.MkdirAll(dst, 0o755)
	copyFile(dbPath, filepath.Join(dst, filepath.Base(dbPath)))
	filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dataDir, path)
		dest := filepath.Join(dst, rel)
		if info.IsDir() {
			os.MkdirAll(dest, info.Mode())
		} else {
			copyFile(path, dest)
		}
		return nil
	})
	log.Printf("snapshot created at %s", dst)
}

func copyFile(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	io.Copy(out, in)
}
