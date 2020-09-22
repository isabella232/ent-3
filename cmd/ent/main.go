package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"strings"

	"os"
	"sort"
	"strconv"
	"time"

	"github.com/filecoin-project/go-state-types/big"
	states0 "github.com/filecoin-project/specs-actors/actors/states"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
	"github.com/filecoin-project/specs-actors/v2/actors/migration"
	"github.com/filecoin-project/specs-actors/v2/actors/states"
	cid "github.com/ipfs/go-cid"
	"github.com/urfave/cli/v2"
	"github.com/zenground0/ent/lib"
	"golang.org/x/xerrors"
)

var rootsCmd = &cli.Command{
	Name:        "roots",
	Description: "provide state tree root cids for migrating",
	Action:      runRootsCmd,
}

var migrateCmd = &cli.Command{
	Name:        "migrate",
	Description: "migrate a filecoin v1 state root to v2",
	Subcommands: []*cli.Command{
		{
			Name:   "one",
			Usage:  "migrate a single state tree",
			Action: runMigrateOneCmd,
		},
		{
			Name:   "chain",
			Usage:  "migrate all state trees from given chain head to genesis",
			Action: runMigrateChainCmd,
			Flags: []cli.Flag{
				&cli.IntFlag{Name: "skip", Aliases: []string{"k"}},
			},
		},
	},
}

var validateCmd = &cli.Command{
	Name:        "validate",
	Description: "validate a migration by checking lots of invariants",
	Action:      runValidateCmd,
}

var debtsCmd = &cli.Command{
	Name:        "debts",
	Description: "display all miner actors in debt and total burnt funds",
	Action:      runDebtsCmd,
}

func main() {
	// pprof server
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	app := &cli.App{
		Name:        "ent",
		Usage:       "Test filecoin state tree migrations by running them",
		Description: "Test filecoin state tree migrations by running them",
		Commands: []*cli.Command{
			migrateCmd,
			validateCmd,
			rootsCmd,
			debtsCmd,
		},
	}
	sort.Sort(cli.CommandsByName(app.Commands))
	for _, c := range app.Commands {
		sort.Sort(cli.FlagsByName(c.Flags))
	}
	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func runMigrateOneCmd(c *cli.Context) error {
	if !c.Args().Present() {
		return xerrors.Errorf("not enough args, need state root to migrate")
	}
	stateRootIn, err := cid.Decode(c.Args().First())
	if err != nil {
		return err
	}
	chn := lib.Chain{}
	store, err := chn.LoadCborStore(c.Context)
	if err != nil {
		return err
	}
	start := time.Now()
	stateRootOut, err := migration.MigrateStateTree(c.Context, store, stateRootIn)
	duration := time.Since(start)
	if err != nil {
		return err
	}
	fmt.Printf("%s => %s -- %v\n", stateRootIn, stateRootOut, duration)
	writeStart := time.Now()
	if err := chn.FlushBufferedState(c.Context, stateRootOut); err != nil {
		return xerrors.Errorf("failed to flush state tree to disk: %w\n", err)
	}
	writeDuration := time.Since(writeStart)
	fmt.Printf("%s buffer flush time: %v\n", stateRootOut, writeDuration)
	return nil
}

func runMigrateChainCmd(c *cli.Context) error {
	if !c.Args().Present() {
		return xerrors.Errorf("not enough args, need chain head to migrate")
	}
	bcid, err := cid.Decode(c.Args().First())
	if err != nil {
		return err
	}
	chn := lib.Chain{}
	iter, err := chn.NewChainStateIterator(c.Context, bcid)
	if err != nil {
		return err
	}
	store, err := chn.LoadCborStore(c.Context)
	if err != nil {
		return err
	}
	k := c.Int("skip")
	for !iter.Done() {
		val := iter.Val()
		if k == 0 || val.Height%int64(k) == int64(0) { // skip every k epochs
			start := time.Now()
			stateRootOut, err := migration.MigrateStateTree(c.Context, store, val.State)
			duration := time.Since(start)
			if err != nil {
				fmt.Printf("%d -- %s => %s !! %v\n", val.Height, val.State, stateRootOut, err)
			} else {
				fmt.Printf("%d -- %s => %s -- %v\n", val.Height, val.State, stateRootOut, duration)
			}
			writeStart := time.Now()
			if err := chn.FlushBufferedState(c.Context, stateRootOut); err != nil {
				fmt.Printf("%s buffer flush failed: %s\n", err, stateRootOut, err)
			}
			writeDuration := time.Since(writeStart)
			fmt.Printf("%s buffer flush time: %v\n", stateRootOut, writeDuration)
		}

		if err := iter.Step(c.Context); err != nil {
			return err
		}
	}
	return nil
}

func runValidateCmd(c *cli.Context) error {
	if !c.Args().Present() {
		return xerrors.Errorf("not enough args, need state root to migrate")
	}
	stateRootIn, err := cid.Decode(c.Args().First())
	if err != nil {
		return err
	}
	chn := lib.Chain{}
	store, err := chn.LoadCborStore(c.Context)
	if err != nil {
		return err
	}

	start := time.Now()
	stateRootOut, err := migration.MigrateStateTree(c.Context, store, stateRootIn)
	duration := time.Since(start)
	if err != nil {
		return err
	}

	fmt.Printf("Migration: %s => %s -- %v\n", stateRootIn, stateRootOut, duration)

	adtStore := adt.WrapStore(c.Context, store)
	actorsOut, err := states0.LoadTree(adtStore, stateRootOut)
	if err != nil {
		return err
	}
	expectedBalance, err := migration.InputTreeBalance(c.Context, store, stateRootIn)
	if err != nil {
		return err
	}
	start = time.Now()
	acc, err := states.CheckStateInvariants(*actorsOut, expectedBalance)
	duration = time.Since(start)
	if err != nil {
		return err
	}
	if acc.IsEmpty() {
		fmt.Printf("Validation: %s -- no errors -- %v\n", stateRootOut, duration)
	} else {
		fmt.Printf("Validation: %s -- errors: %s -- %v\n", stateRootOut, strings.Join(acc.Messages(), ", "), duration)
	}

	return nil
}

func runRootsCmd(c *cli.Context) error {
	if c.Args().Len() < 2 {
		return xerrors.Errorf("not enough args, need chain tip and number of states to fetch")
	}

	bcid, err := cid.Decode(c.Args().First())
	if err != nil {
		return err
	}
	num, err := strconv.Atoi(c.Args().Get(1))
	if err != nil {
		return err
	}
	// Read roots from lotus datastore
	roots := make([]cid.Cid, num)
	chn := lib.Chain{}
	iter, err := chn.NewChainStateIterator(c.Context, bcid)
	if err != nil {
		return err
	}
	for i := 0; !iter.Done() && i < num; i++ {
		roots[i] = iter.Val().State
		if err := iter.Step(c.Context); err != nil {
			return err
		}
	}
	// Output roots
	for _, root := range roots {
		fmt.Printf("%s\n", root)
	}
	return nil
}

func runDebtsCmd(c *cli.Context) error {
	if !c.Args().Present() {
		return xerrors.Errorf("not enough args, need state root to migrate")
	}
	stateRootIn, err := cid.Decode(c.Args().First())
	if err != nil {
		return err
	}
	chn := lib.Chain{}
	store, err := chn.LoadCborStore(c.Context)
	if err != nil {
		return err
	}

	bf, err := migration.InputTreeBurntFunds(c.Context, store, stateRootIn)
	if err != nil {
		return err
	}

	available, err := migration.InputTreeMinerAvailableBalance(c.Context, store, stateRootIn)
	if err != nil {
		return err
	}
	// filter out positive balances
	totalDebt := big.Zero()
	for addr, balance := range available {
		if balance.LessThan(big.Zero()) {
			debt := balance.Neg()
			fmt.Printf("miner %s: %s\n", addr, debt)
			totalDebt = big.Add(totalDebt, debt)
		}
	}
	fmt.Printf("burnt funds balance: %s\n", bf)
	fmt.Printf("total debt:          %s\n", totalDebt)
	return nil
}
