package addfs

import (
	"context"
	"fmt"
	"time"

	"github.com/grailbio/base/file/fsnode"
	"github.com/grailbio/base/log"
)

type (
	// PerNodeFunc computes nodes to add to a directory tree, for example to present alternate views
	// of raw data, expand archive files, etc. It operates on a single node at a time. If it returns
	// any "addition" nodes, ApplyPerNodeFuncs will place them under a sibling directory called
	// "...". For example, suppose we have an input directory:
	//   parent/
	//   └─dir1/
	//     ├─fileA
	//     ├─fileB
	//     └─dir2/
	// and we call ApplyPerNodeFuncs(parent/, ourFns). The resulting directory tree will be
	//   parent/
	//   ├─.../
	//   │ └─dir1/
	//   │   └─[ nodes returned by PerNodeFunc.Apply(_, dir1/) for all ourFns ]
	//   └─dir1/
	//     ├─.../
	//     │ ├─fileA/
	//     │ │ └─[ nodes returned by PerNodeFunc.Apply(_, fileA) for all ourFns ]
	//     │ ├─fileB/
	//     │ │ └─[ nodes returned by PerNodeFunc.Apply(_, fileB) for all ourFns ]
	//     │ └─dir2/
	//     │   └─[ nodes returned by PerNodeFunc.Apply(_, dir2/) for all ourFns ]
	//     ├─fileA
	//     ├─fileB
	//     └─dir2/
	//       └─.../
	// Users browsing this resulting tree can work with just the original files and ourFns won't
	// be invoked. However, they can also navigate into any of the .../s if interested and then
	// use the additional views generated by ourFns. If they're interested in our_view for
	// /path/to/a/file, they just need to prepend .../, like /path/to/a/.../file/our_view.
	// (Perhaps it'd be more intuitive to "append", like /path/to/a/file/our_view, but then the
	// file name would conflict with the view-containing directory.)
	//
	// Funcs that need to list the children of a fsnode.Parent should be careful: they may want to
	// set an upper limit on number of entries to read, and otherwise default to empty, to avoid
	// performance problems (resulting in bad UX) for very large directories.
	//
	// Funcs that simply look at filenames and declare derived outputs may want to place their
	// children directly under /.../file/ for convenient access. However, Funcs that are expensive,
	// for example reading some file contents, etc., may want to separate themselves under their own
	// subdirectory, like .../file/func_name/. This lets users browsing the tree "opt-in" to seeing
	// the results of the expensive computation by navigating to .../file/func_name/.
	//
	// If the input tree has any "..." that conflict with the added ones, the added ones override.
	// The originals will simply not be accessible.
	PerNodeFunc interface {
		Apply(context.Context, fsnode.T) (adds []fsnode.T, _ error)
	}
	perNodeFunc func(context.Context, fsnode.T) (adds []fsnode.T, _ error)
)

func NewPerNodeFunc(fn func(context.Context, fsnode.T) ([]fsnode.T, error)) PerNodeFunc {
	return perNodeFunc(fn)
}
func (f perNodeFunc) Apply(ctx context.Context, n fsnode.T) ([]fsnode.T, error) { return f(ctx, n) }

const addsDirName = "..."

// perNodeImpl extends the original Parent with the .../ child.
type perNodeImpl struct {
	fsnode.Parent
	fns  []PerNodeFunc
	adds fsnode.Parent
}

var (
	_ fsnode.Parent    = (*perNodeImpl)(nil)
	_ fsnode.Cacheable = (*perNodeImpl)(nil)
)

// ApplyPerNodeFuncs returns a new Parent that contains original's nodes plus any added by fns.
// See PerNodeFunc's for more documentation on how this works.
// Later fns's added nodes will overwrite earlier ones, if any names conflict.
func ApplyPerNodeFuncs(original fsnode.Parent, fns ...PerNodeFunc) fsnode.Parent {
	fns = append([]PerNodeFunc{}, fns...)
	adds := perNodeAdds{
		FileInfo: fsnode.CopyFileInfo(original.Info()).WithName(addsDirName),
		original: original,
		fns:      fns,
	}
	return &perNodeImpl{original, fns, &adds}
}

func (n *perNodeImpl) CacheableFor() time.Duration { return fsnode.CacheableFor(n.Parent) }
func (n *perNodeImpl) Child(ctx context.Context, name string) (fsnode.T, error) {
	if name == addsDirName {
		return n.adds, nil
	}
	child, err := n.Parent.Child(ctx, name)
	if err != nil {
		return nil, err
	}
	return perNodeRecurse(child, n.fns), nil
}
func (n *perNodeImpl) Children() fsnode.Iterator {
	return fsnode.NewConcatIterator(
		// TODO: Consider omitting .../ if the directory has no other children.
		fsnode.NewIterator(n.adds),
		// TODO: Filter out any conflicting ... to be consistent with Child.
		fsnode.MapIterator(n.Parent.Children(), func(_ context.Context, child fsnode.T) (fsnode.T, error) {
			return perNodeRecurse(child, n.fns), nil
		}),
	)
}

// perNodeAdds is the .../ Parent. It has a child (directory) for each original child (both
// directories and files). The children contain the PerNodeFunc.Apply outputs.
type perNodeAdds struct {
	fsnode.ParentReadOnly
	fsnode.FileInfo
	original fsnode.Parent
	fns      []PerNodeFunc
}

var (
	_ fsnode.Parent    = (*perNodeAdds)(nil)
	_ fsnode.Cacheable = (*perNodeAdds)(nil)
)

func (n *perNodeAdds) Child(ctx context.Context, name string) (fsnode.T, error) {
	child, err := n.original.Child(ctx, name)
	if err != nil {
		return nil, err
	}
	return n.newAddsForChild(child), nil
}
func (n *perNodeAdds) Children() fsnode.Iterator {
	// TODO: Filter out any conflicting ... to be consistent with Child.
	return fsnode.MapIterator(n.original.Children(), func(_ context.Context, child fsnode.T) (fsnode.T, error) {
		return n.newAddsForChild(child), nil
	})
}
func (n *perNodeAdds) FSNodeT() {}

func (n *perNodeAdds) newAddsForChild(original fsnode.T) fsnode.Parent {
	originalInfo := original.Info()
	return fsnode.NewParent(
		fsnode.NewDirInfo(originalInfo.Name()).
			WithModTime(originalInfo.ModTime()).
			// Derived directory must be executable to be usable, even if original file wasn't.
			WithModePerm(originalInfo.Mode().Perm()|0111).
			WithCacheableFor(fsnode.CacheableFor(original)),
		fsnode.FuncChildren(func(ctx context.Context) ([]fsnode.T, error) {
			adds := make(map[string]fsnode.T)
			for _, fn := range n.fns {
				fnAdds, err := fn.Apply(ctx, original)
				if err != nil {
					return nil, fmt.Errorf("addfs: error running func %v: %w", fn, err)
				}
				for _, add := range fnAdds {
					name := add.Info().Name()
					if _, exists := adds[name]; exists {
						// TODO: Consider returning an error here. Or merging the added trees?
						log.Error.Printf("addfs %s: conflict for added name: %s", originalInfo.Name(), name)
					}
					adds[name] = add
				}
			}
			wrapped := make([]fsnode.T, 0, len(adds))
			for _, add := range adds {
				wrapped = append(wrapped, perNodeRecurse(add, n.fns))
			}
			return wrapped, nil
		}),
	)
}

func perNodeRecurse(node fsnode.T, fns []PerNodeFunc) fsnode.T {
	parent, ok := node.(fsnode.Parent)
	if !ok {
		return node
	}
	return ApplyPerNodeFuncs(parent, fns...)
}
