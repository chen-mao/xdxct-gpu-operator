package ocidir

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/regclient/regclient/scheme"
	"github.com/regclient/regclient/types"
	"github.com/regclient/regclient/types/ref"
	"github.com/regclient/regclient/types/tag"
)

// TagDelete removes a tag from the repository
func (o *OCIDir) TagDelete(ctx context.Context, r ref.Ref) error {
	if r.Tag == "" {
		return types.ErrMissingTag
	}
	// get index
	index, err := o.readIndex(r)
	if err != nil {
		return fmt.Errorf("failed to read index: %w", err)
	}
	changed := false
	for i, desc := range index.Manifests {
		if t, ok := desc.Annotations[aOCIRefName]; ok && t == r.Tag {
			// remove matching entry from index
			index.Manifests = append(index.Manifests[:i], index.Manifests[i+1:]...)
			changed = true
		}
	}
	if !changed {
		return fmt.Errorf("failed deleting %s: %w", r.CommonName(), types.ErrNotFound)
	}
	// push manifest back out
	err = o.writeIndex(r, index)
	if err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}
	o.refMod(r)
	return nil
}

// TagList returns a list of tags from the repository
func (o *OCIDir) TagList(ctx context.Context, r ref.Ref, opts ...scheme.TagOpts) (*tag.List, error) {
	// get index
	index, err := o.readIndex(r)
	if err != nil {
		return nil, err
	}
	tl := []string{}
	for _, desc := range index.Manifests {
		if t, ok := desc.Annotations[aOCIRefName]; ok {
			if i := strings.LastIndex(t, ":"); i >= 0 {
				t = t[i+1:]
			}
			found := false
			for _, cur := range tl {
				if cur == t {
					found = true
					break
				}
			}
			if !found {
				tl = append(tl, t)
			}
		}
	}
	sort.Strings(tl)
	ib, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	// return listing from index
	t, err := tag.New(
		tag.WithRaw(ib),
		tag.WithRef(r),
		tag.WithMT(types.MediaTypeOCI1ManifestList),
		tag.WithTags(tl),
	)
	if err != nil {
		return nil, err
	}
	return t, nil
}
