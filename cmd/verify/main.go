package main

import (
	"flag"
	"fmt"
	"os"

	"clockql/internal/verify"
)

func main() {
	cfg := verify.DefaultConfig()
	flag.Int64Var(&cfg.Seed, "seed", cfg.Seed, "random seed")
	flag.IntVar(&cfg.ValidCount, "valid", cfg.ValidCount, "number of valid queries")
	flag.IntVar(&cfg.BadCount, "bad", cfg.BadCount, "number of invalid queries")
	flag.IntVar(&cfg.Datasets, "datasets", cfg.Datasets, "datasets per valid query")
	flag.IntVar(&cfg.MaxFailures, "max-failures", cfg.MaxFailures, "failures to retain")
	jsonOut := flag.String("json", "", "write the full result as JSON to this path")
	flag.Parse()

	res := verify.Run(cfg)
	fmt.Print(res.Summary())
	if *jsonOut != "" {
		if err := verify.WriteJSON(res, *jsonOut); err != nil {
			fmt.Fprintln(os.Stderr, "write json:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", *jsonOut)
	}
	if res.ValidOK != res.Valid || res.BadOK != res.Bad {
		os.Exit(1)
	}
}
