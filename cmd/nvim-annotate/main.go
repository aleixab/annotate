// Command nvim-annotate is the storage/git/remap engine behind the annotate.nvim
// plugin. Every subcommand reads a JSON request on stdin and writes a JSON
// response on stdout; errors are written as {"error": "..."} and exit non-zero.
// It has no knowledge of Neovim and is fully usable as a standalone CLI.
//
// Usage: nvim-annotate <add|list|edit|delete|search|reanchor|prune|export|import|stats>
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aleixab/nvim-annotate/internal/core"
	"github.com/aleixab/nvim-annotate/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: nvim-annotate <add|list|edit|delete|search|reanchor|prune|export|import|stats>")
	}

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail("reading stdin: " + err.Error())
	}

	dbPath, err := databasePath()
	if err != nil {
		fail(err.Error())
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fail("opening database: " + err.Error())
	}
	defer st.Close()

	switch os.Args[1] {
	case "add":
		var req core.AddRequest
		decode(in, &req)
		id, err := core.Add(st, req)
		check(err)
		emit(core.AddResponse{ID: id})

	case "list":
		var req core.ListRequest
		decode(in, &req)
		resp, err := core.List(st, req)
		check(err)
		emit(resp)

	case "edit":
		var req core.EditRequest
		decode(in, &req)
		check(core.Edit(st, req))
		emit(core.OKResponse{OK: true})

	case "delete":
		var req core.DeleteRequest
		decode(in, &req)
		check(core.Delete(st, req))
		emit(core.OKResponse{OK: true})

	case "search":
		var req core.SearchRequest
		decode(in, &req)
		resp, err := core.Search(st, req)
		check(err)
		emit(resp)

	case "reanchor":
		var req core.ReanchorRequest
		decode(in, &req)
		check(core.Reanchor(st, req))
		emit(core.OKResponse{OK: true})

	case "prune":
		var req core.PruneRequest
		decode(in, &req)
		resp, err := core.Prune(st, req)
		check(err)
		emit(resp)

	case "export":
		resp, err := core.Export(st)
		check(err)
		emit(resp)

	case "import":
		var req core.ImportRequest
		decode(in, &req)
		resp, err := core.Import(st, req)
		check(err)
		emit(resp)

	case "stats":
		resp, err := core.Stats(st, dbPath)
		check(err)
		emit(resp)

	default:
		fail("unknown command: " + os.Args[1])
	}
}

// databasePath returns the XDG data path for the shared DB, creating its
// directory. It lives deliberately outside any repo so it can never be committed.
func databasePath() (string, error) {
	dir := os.Getenv("NVIM_ANNOTATE_DB_DIR")
	if dir == "" {
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			data = filepath.Join(home, ".local", "share")
		}
		dir = filepath.Join(data, "nvim-annotations")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "notes.db"), nil
}

func decode(in []byte, v any) {
	if len(in) == 0 {
		return
	}
	if err := json.Unmarshal(in, v); err != nil {
		fail("invalid JSON request: " + err.Error())
	}
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(v); err != nil {
		fail("encoding response: " + err.Error())
	}
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

// fail writes a JSON error object to stdout and exits non-zero.
func fail(msg string) {
	out, _ := json.Marshal(map[string]string{"error": msg})
	fmt.Fprintln(os.Stdout, string(out))
	os.Exit(1)
}
