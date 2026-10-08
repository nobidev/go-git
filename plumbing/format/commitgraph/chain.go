package commitgraph

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path"

	"github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/plumbing"
)

// OpenChainFile reads a commit chain file and returns a slice of the hashes within it
//
// Commit-Graph chains are described at https://git-scm.com/docs/commit-graph
// and are new line separated list of graph file hashes, oldest to newest.
//
// This function simply reads the file and returns the hashes as a slice.
func OpenChainFile(r io.Reader, opts ...Option) ([]string, error) {
	o, err := readOptions(opts)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, io.ErrUnexpectedEOF
	}
	bufRd := bufio.NewReader(r)
	chain := make([]string, 0, 8)
	for {
		line, err := bufRd.ReadSlice('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		hashStr := string(line[:len(line)-1])
		if len(hashStr) != o.objectFormat.HexSize() || !plumbing.IsHash(hashStr) {
			return nil, ErrMalformedCommitGraphFile
		}
		chain = append(chain, hashStr)
	}
	return chain, nil
}

// OpenChainOrFileIndex expects a billy.Filesystem representing a .git directory.
// It will first attempt to read a commit-graph index file, before trying to read a
// commit-graph chain file and its index files. If neither are present, an error is returned.
// Otherwise an Index will be returned.
//
// See: https://git-scm.com/docs/commit-graph
func OpenChainOrFileIndex(fs billy.Filesystem, opts ...Option) (Index, error) {
	if _, err := readOptions(opts); err != nil {
		return nil, err
	}
	file, err := fs.Open(path.Join("objects", "info", "commit-graph"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OpenChainIndex(fs, opts...)
		}
		return nil, err
	}

	index, err := OpenFileIndex(file, opts...)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return index, nil
}

// OpenChainIndex expects a billy.Filesystem representing a .git directory.
// It will read a commit-graph chain file and return a coalesced index.
// If the chain file is not present or names no graphs, or a graph in that
// chain is not present or invalid, an error is returned.
//
// See: https://git-scm.com/docs/commit-graph
func OpenChainIndex(fs billy.Filesystem, opts ...Option) (Index, error) {
	if _, err := readOptions(opts); err != nil {
		return nil, err
	}
	chainFile, err := fs.Open(path.Join("objects", "info", "commit-graphs", "commit-graph-chain"))
	if err != nil {
		return nil, err
	}

	chain, err := OpenChainFile(chainFile, opts...)
	_ = chainFile.Close()
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, ErrMalformedCommitGraphFile
	}

	var index Index
	for i, hash := range chain {
		if p, ok := index.(*fileIndex); ok && p.graphOID.String() != chain[i-1] {
			_ = index.Close()
			return nil, ErrMalformedCommitGraphFile
		}
		file, err := fs.Open(path.Join("objects", "info", "commit-graphs", "graph-"+hash+".graph"))
		if err != nil {
			// Ignore closing errors and return the error from opening the file instead
			if index != nil {
				_ = index.Close()
			}
			return nil, err
		}

		next, err := OpenFileIndexWithParent(file, index, opts...)
		if err != nil {
			// Ignore closing errors and return the error from OpenFileIndexWithParent instead
			_ = file.Close()
			if index != nil {
				_ = index.Close()
			}
			return nil, err
		}
		index = next
	}

	return index, nil
}
