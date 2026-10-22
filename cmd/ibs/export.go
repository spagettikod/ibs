package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ibs/internal/site"
)

// runExport is "ibs export": the site's files, from the recovery phrase.
func runExport(dataDir string, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	index := fs.String("index", filepath.Join(dataDir, "ibs.db"), "the site's database, or a metadata backup of it (from a node or another site)")
	blocks := fs.String("blocks", filepath.Join(dataDir, "blocks"), "folders with the site's blocks, comma separated: the site's, its nodes', other sites' copies")
	bucket := fs.String("bucket", "", "export only this bucket")
	printKeys := fs.Bool("keys", false, "only print the site's and the buckets' age keys, to use the age tool yourself")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ibs export [flags] OUT\n\nWrites every file to OUT/<bucket>/<key>. The recovery phrase is read from\nIBS_RECOVERY_PHRASE, or asked for.\n\nflags:")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 && !*printKeys {
		fs.Usage()
		return errors.New("give the folder to export to")
	}
	phrase := os.Getenv("IBS_RECOVERY_PHRASE")
	if phrase == "" {
		fmt.Fprint(os.Stderr, "Recovery phrase: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		phrase = strings.TrimSpace(line)
	}
	res, err := site.Export(site.ExportOptions{
		Phrase: phrase, Index: *index, Blocks: strings.Split(*blocks, ","), Bucket: *bucket,
		Out: fs.Arg(0), PrintKeys: *printKeys, Log: os.Stdout,
	})
	if err != nil || *printKeys {
		return err
	}
	fmt.Printf("exported %d files, %d bytes, to %s\n", res.Files, res.Bytes, fs.Arg(0))
	if len(res.Missing) > 0 {
		return fmt.Errorf("%d files couldn't be exported (listed above): their blocks are missing or damaged; add the folders of nodes or other sites that hold them", len(res.Missing))
	}
	return nil
}
