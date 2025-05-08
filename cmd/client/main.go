// cmd/client/main.go – AVID‑FP Object Store client (v2.4.0, May 2025)
// Supports either the old flag set (-peers -m -n) or a YAML file
// loaded via -config, just like the server.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dattu/distributed_object_store/pkg/config"
	"github.com/dattu/distributed_object_store/pkg/erasure"
	"github.com/dattu/distributed_object_store/pkg/fingerprint"
	"github.com/dattu/distributed_object_store/pkg/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

/* -------------------------------------------------------------------- */
/* entry‑point                                                          */
/* -------------------------------------------------------------------- */

func main() {
	/* -------- flags -------- */
	cfgPath   := flag.String("config", "", "YAML config file (optional)")
	mode      := flag.String("mode", "disperse", "disperse | retrieve")
	filePath  := flag.String("file", "", "Path to input (disperse) or output (retrieve)")
	objectID  := flag.String("id", "", "Unique object ID")
	peersFlag := flag.String("peers", "", "Comma‑separated host:port list (override)")
	mFlag     := flag.Int("m", 0, "data shards (override)")
	nFlag     := flag.Int("n", 0, "total shards (override)")
	flag.Parse()

	/* -------- load YAML if given -------- */
	var (
		peers []string
		m, n  int
	)
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		peers = append([]string{}, cfg.Cluster.Peers...)
		m, n = cfg.Erasure.Data, cfg.Erasure.Total
	}

	/* -------- CLI overrides win -------- */
	if *peersFlag != "" {
		peers = strings.Split(*peersFlag, ",")
	}
	if *mFlag != 0 {
		m = *mFlag
	}
	if *nFlag != 0 {
		n = *nFlag
	}

	/* -------- sanity checks -------- */
	if *objectID == "" || *filePath == "" || len(peers) == 0 || m == 0 || n == 0 {
		log.Fatalf("need -id, -file, and peers/m/n via flags or -config")
	}

	f := n - m

	switch *mode {
	case "disperse":
		if pingPeers(peers) < 2*f {
			log.Fatalf("quorum impossible: need ≥%d reachable peers", 2*f)
		}
		disperse(peers, *filePath, *objectID, m, n)
	case "retrieve":
		retrieve(peers, *filePath, *objectID, m, n)
	default:
		log.Fatalf("unknown mode %q; must be disperse or retrieve", *mode)
	}
}

/* -------------------------------------------------------------------- */
/* helpers: network / erasure                                            */
/* -------------------------------------------------------------------- */

func pingPeers(peers []string) int {
	cnt := 0
	for _, p := range peers {
		if c, err := net.DialTimeout("tcp", p, 2*time.Second); err == nil {
			cnt++
			c.Close()
		}
	}
	return cnt
}

func disperse(servers []string, path, id string, m, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("ReadFile: %v", err)
	}
	enc, err := erasure.New(m, n)
	if err != nil {
		log.Fatalf("erasure.New: %v", err)
	}
	shards, _, err := enc.Encode(data)
	if err != nil {
		log.Fatalf("Encode: %v", err)
	}

	fpGen, _ := fingerprint.NewRandom()
	hashes := make([][]byte, n)
	fps := make([]uint64, n)
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, sh := range shards {
		i, sh := i, sh
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			h := sha256.Sum256(sh)
			hashes[i] = h[:]
			fps[i] = fpGen.Eval(sh)
		}()
	}
	wg.Wait()

	fpcc := &protocol.FPCC{Hashes: hashes, Fps: fps, Seed: fpGen.Seed()}

	for i, shard := range shards {
		req := &protocol.DisperseRequest{
			ObjectId:       id,
			FragmentIndex:  uint32(i),
			Fragment:       shard,
			Fpcc:           fpcc,
		}
		var wgSend sync.WaitGroup
		wgSend.Add(len(servers))
		for _, addr := range servers {
			go func(a string) { defer wgSend.Done(); fanOutShard(a, req) }(strings.TrimSpace(addr))
		}
		wgSend.Wait()
		fmt.Printf("Shard %d/%d dispersed\n", i+1, n)
	}
	fmt.Printf("Disperse complete for %q\n", id)
}

func fanOutShard(addr string, req *protocol.DisperseRequest) {
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancel()
		if err != nil {
			log.Printf("dial %s failed (%d/3): %v", addr, attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}
		c := protocol.NewDispersalClient(conn)
		rCtx, rCancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := c.Disperse(rCtx, req)
		rCancel()
		conn.Close()
		if err != nil || !resp.Ok {
			log.Printf("disperse to %s failed (%d/3): %v / %s", addr, attempt, err, resp.GetError())
			time.Sleep(2 * time.Second)
			continue
		}
		return
	}
	log.Fatalf("shard %d → %s failed after 3 attempts", req.FragmentIndex, addr)
}

func retrieve(servers []string, out, id string, m, n int) {
	ctx := context.Background()
	connPool := make(map[string]*grpc.ClientConn)
	clientFor := func(addr string) (protocol.DispersalClient, error) {
		if c, ok := connPool[addr]; ok {
			return protocol.NewDispersalClient(c), nil
		}
		c, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithTimeout(15*time.Second))
		if err != nil {
			return nil, err
		}
		connPool[addr] = c
		return protocol.NewDispersalClient(c), nil
	}

	// 1) fetch FPCC + shard‑0
	var fpcc *protocol.FPCC
	shards := make([][]byte, n)
	var fpGen *fingerprint.Fingerprint
	for _, addr := range servers {
		client, err := clientFor(strings.TrimSpace(addr))
		if err != nil {
			continue
		}
		r0, err := client.Retrieve(ctx, &protocol.RetrieveRequest{ObjectId: id, FragmentIndex: 0})
		if err != nil || !r0.Ok {
			continue
		}
		// verify shard‑0
		h0 := sha256.Sum256(r0.Fragment)
		if !bytes.Equal(h0[:], r0.Fpcc.Hashes[0]) ||
			fingerprint.NewWithSeed(r0.Fpcc.Seed).Eval(r0.Fragment) != r0.Fpcc.Fps[0] {
			continue
		}
		fpcc = r0.Fpcc
		fpGen = fingerprint.NewWithSeed(fpcc.Seed)
		shards[0] = r0.Fragment
		break
	}
	if fpcc == nil {
		log.Fatalf("failed to retrieve a valid shard‑0 from any peer")
	}

	// 2) fetch other shards
	received := 1
	for idx := 1; idx < n && received < m; idx++ {
		for _, addr := range servers {
			client, err := clientFor(strings.TrimSpace(addr))
			if err != nil {
				continue
			}
			r, err := client.Retrieve(ctx, &protocol.RetrieveRequest{ObjectId: id, FragmentIndex: uint32(idx)})
			if err != nil || !r.Ok {
				continue
			}
			h := sha256.Sum256(r.Fragment)
			if !bytes.Equal(h[:], fpcc.Hashes[idx]) || fpGen.Eval(r.Fragment) != fpcc.Fps[idx] {
				continue
			}
			shards[idx] = r.Fragment
			received++
			break
		}
	}
	if received < m {
		log.Fatalf("only %d/%d good shards; cannot decode", received, m)
	}

	// 3) decode and write file
	enc, _ := erasure.New(m, n)
	raw, err := enc.Decode(shards, len(shards[0])*m)
	if err != nil {
		log.Fatalf("Decode: %v", err)
	}
	data := bytes.TrimRight(raw, "\x00")
	if err := os.WriteFile(out, data, 0644); err != nil {
		log.Fatalf("WriteFile: %v", err)
	}
	fmt.Printf("Retrieved %q → %q\n", id, out)
}
