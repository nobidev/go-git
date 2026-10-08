package commitgraph

import (
	"crypto"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

// Option configures a commit-graph reader or encoder.
type Option func(*options)

type options struct {
	objectFormat formatcfg.ObjectFormat
}

// WithObjectFormat selects the repository's object format. The default, including
// UnsetObjectFormat, is SHA-1. Readers reject graphs with a different hash version.
func WithObjectFormat(of formatcfg.ObjectFormat) Option {
	return func(o *options) { o.objectFormat = of }
}

func readOptions(opts []Option) (options, error) {
	o := options{objectFormat: formatcfg.DefaultObjectFormat}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.objectFormat == formatcfg.UnsetObjectFormat {
		o.objectFormat = formatcfg.DefaultObjectFormat
	}
	if o.objectFormat != formatcfg.SHA1 && o.objectFormat != formatcfg.SHA256 {
		return o, formatcfg.ErrInvalidObjectFormat
	}
	return o, nil
}

func (o options) hashAlgorithm() crypto.Hash {
	if o.objectFormat == formatcfg.SHA256 {
		return crypto.SHA256
	}
	return crypto.SHA1
}
