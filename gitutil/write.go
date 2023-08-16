// Copyright 2023 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gitutil

import (
	"bytes"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

func SaveBlob(st storer.EncodedObjectStorer, data []byte) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	w, err := enc.Writer()
	if err != nil {
		return
	}
	if _, err := w.Write(data); err != nil {
		return id, err
	}
	if err := w.Close(); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}

func SaveConfig(st storer.EncodedObjectStorer, cfg *config.Config) (id plumbing.Hash, err error) {
	var buf bytes.Buffer
	if err = config.NewEncoder(&buf).Encode(cfg); err != nil {
		return
	}

	return SaveBlob(st, buf.Bytes())
}

func SaveTree(st storer.EncodedObjectStorer, entries []object.TreeEntry) (id plumbing.Hash, err error) {
	SortTreeEntries(entries)

	enc := st.NewEncodedObject()
	enc.SetType(plumbing.TreeObject)

	t := object.Tree{Entries: entries}
	if err := t.Encode(enc); err != nil {
		return id, err
	}

	return st.SetEncodedObject(enc)
}

func SaveCommit(st storer.EncodedObjectStorer, c *object.Commit) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.CommitObject)
	if err := c.Encode(enc); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}

// TestMapToEntries provides input to PatchTree. keys are filenames, with
// suffixes:
// * '!' = delete
// * '*' = executable
// * '@' = symlink
// * '#' = submodule.
func TestMapToEntries(st storer.EncodedObjectStorer, in map[string]string) ([]object.TreeEntry, error) {
	var es []object.TreeEntry
	for k, v := range in {
		id, err := SaveBlob(st, []byte(v))
		if err != nil {
			return nil, err
		}
		mode := filemode.Regular

		last := k[len(k)-1]
		trim := true
		switch last {
		case '*':
			mode = filemode.Executable
		case '@':
			mode = filemode.Symlink
		case '!':
			id = plumbing.ZeroHash
		case '#':
			mode = filemode.Submodule
			id = plumbing.NewHash(v)
		default:
			trim = false
		}
		if trim {
			k = k[:len(k)-1]
		}

		es = append(es, object.TreeEntry{Name: k, Hash: id, Mode: mode})
	}
	return es, nil
}

func ModifyCommit(st storer.EncodedObjectStorer, c *object.Commit, newContent map[string]string, message string) (id plumbing.Hash, err error) {
	tree, err := object.GetTree(st, c.TreeHash)

	es, err := TestMapToEntries(st, newContent)
	if err != nil {
		return id, err
	}

	treeID, err := PatchTree(st, tree, es)
	if err != nil {
		return id, err
	}

	newCommit := object.Commit{
		Message:      message,
		TreeHash:     treeID,
		ParentHashes: []plumbing.Hash{c.Hash},
	}

	return SaveCommit(st, &newCommit)
}
