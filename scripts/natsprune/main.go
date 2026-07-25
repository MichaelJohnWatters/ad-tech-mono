// natsprune — one-off recovery tool: delete orphaned per-pod JetStream
// consumers whose owning pod no longer exists. Reads consumer names (one per
// line) from the file given as arg 1 and deletes each from the "adtech" stream.
//
//	kubectl -n adtech port-forward svc/nats 4222:4222 &
//	go run ./scripts/natsprune /tmp/orphans.txt
//
// Safe to delete after use.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: natsprune <names-file>")
		os.Exit(1)
	}
	names := []string{}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if s := sc.Text(); s != "" {
			names = append(names, s)
		}
	}
	f.Close()
	fmt.Printf("loaded %d consumer names to delete\n", len(names))

	nc, err := nats.Connect("nats://localhost:4222", nats.Timeout(10*time.Second))
	if err != nil {
		panic(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		panic(err)
	}

	var deleted, missing, failed int64
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := js.DeleteConsumer(ctx, "adtech", name)
				cancel()
				switch {
				case err == nil:
					n := atomic.AddInt64(&deleted, 1)
					if n%100 == 0 {
						fmt.Printf("  deleted %d...\n", n)
					}
				case err == jetstream.ErrConsumerNotFound:
					atomic.AddInt64(&missing, 1)
				default:
					atomic.AddInt64(&failed, 1)
					fmt.Printf("  FAIL %s: %v\n", name, err)
				}
			}
		}()
	}
	for _, n := range names {
		jobs <- n
	}
	close(jobs)
	wg.Wait()
	fmt.Printf("done: deleted=%d already-gone=%d failed=%d\n", deleted, missing, failed)
}
