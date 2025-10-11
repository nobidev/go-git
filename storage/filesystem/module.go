package filesystem

import (
	"github.com/nobidev/go-git/v6/plumbing/cache"
	"github.com/nobidev/go-git/v6/storage"
	"github.com/nobidev/go-git/v6/storage/filesystem/dotgit"
)

type ModuleStorage struct {
	dir *dotgit.DotGit
}

func (s *ModuleStorage) Module(name string) (storage.Storer, error) {
	fs, err := s.dir.Module(name)
	if err != nil {
		return nil, err
	}

	return NewStorage(fs, cache.NewObjectLRUDefault()), nil
}
