// Dumps the shuffled prompt order for a corpus as a JSON array on stdout,
// using the same PCG seeding and Fisher-Yates shuffle as cache_bench_test.go.
// Lets the Python sweep read the canonical order without reimplementing Go's RNG.
package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
)

type Corpus struct {
	Seed     int       `json:"seed"`
	Clusters []Cluster `json:"clusters"`
}

type Cluster struct {
	Prompts []string `json:"prompts"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dumporder <corpus.json>")
		os.Exit(2)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read corpus: %v\n", err)
		os.Exit(1)
	}

	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Fprintf(os.Stderr, "parse corpus: %v\n", err)
		os.Exit(1)
	}

	prompts := make([]string, 0, len(c.Clusters)*5)
	for _, cl := range c.Clusters {
		prompts = append(prompts, cl.Prompts...)
	}

	seed := uint64(c.Seed)
	rng := rand.New(rand.NewPCG(seed, seed))
	rng.Shuffle(len(prompts), func(i, j int) {
		prompts[i], prompts[j] = prompts[j], prompts[i]
	})

	if err := json.NewEncoder(os.Stdout).Encode(prompts); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
}
