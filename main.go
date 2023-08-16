//    Copyright 2023, Google LLC
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
//
package main

import (
	"crypto/sha1"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/hanwen/allusersync/gitutil"
	gerrit "github.com/hanwen/go-gerrit"
)

type AccountInfo struct {
	account gerrit.AccountDetailInfo
	extIDs  []gerrit.AccountExternalIdInfo
}

func getAccountDetails(cl *gerrit.Client, id string) (*AccountInfo, error) {
	details, reply, err := cl.Accounts.GetAccountDetails(id)

	if reply != nil && reply.StatusCode == 404 {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}
	extIDs, _, err := cl.Accounts.GetAccountExternalIDs(id)
	if err != nil {
		return nil, err
	}

	return &AccountInfo{
		account: *details,
		extIDs:  extIDs,
	}, nil
}

type RefUpdate struct {
	// TODO - OldID plumbing.Hash
	NewID plumbing.Hash
}

type RefTransaction struct {
	updates map[string]*RefUpdate
}

func UpdateRepo(ref storer.ReferenceStorer, tr *RefTransaction) error {
	// go-git doesn't do transactions.
	for name, update := range tr.updates {
		if update == nil {
			ref.RemoveReference(plumbing.ReferenceName(name))
			continue
		}
		n := plumbing.NewHashReference(plumbing.ReferenceName(name), update.NewID)
		if err := ref.SetReference(n); err != nil {
			return err
		}
	}
	return nil
}

func newSig() object.Signature {
	return object.Signature{
		Name:  "allusersync",
		Email: "allusersync@invalid",
		When:  time.Now(),
	}
}

func saveAccountDetails(infos []*AccountInfo, repo *git.Repository) error {
	s := newSig()
	extRefName := "refs/meta/external-ids"
	extRef, err := repo.Reference(plumbing.ReferenceName(extRefName), true)
	var extCommit *object.Commit
	if err == plumbing.ErrReferenceNotFound {
		err = nil
	}
	if err != nil {
		return err
	}

	if extRef != nil {
		extCommit, err = repo.CommitObject(extRef.Hash())
		if err != nil {
			return err
		}
	}

	var newEntries []object.TreeEntry

	trans := &RefTransaction{
		updates: map[string]*RefUpdate{},
	}

	for _, inf := range infos {
		cfg := &config.Config{}

		cfg.SetOption("account", "", "fullName", inf.account.Name)
		cfg.SetOption("account", "", "preferredEmail", inf.account.Email)

		id, err := gitutil.SaveConfig(repo.Storer, cfg)
		if err != nil {
			return err
		}

		// TODO - read previous state, and drop associated external ids.
		id, err = gitutil.SaveTree(repo.Storer, []object.TreeEntry{
			{
				Name: "account.config",
				Mode: filemode.Regular,
				Hash: id,
			}})
		if err != nil {
			return err
		}

		// TODO - could work registration date into Author/committer timestamp
		id, err = gitutil.SaveCommit(
			repo.Storer, &object.Commit{
				Author:    s,
				Committer: s,
				Message:   "update account",
				TreeHash:  id,
				// TODO - set parent.
			})
		if err != nil {
			return err
		}

		uidRef := fmt.Sprintf("refs/users/%02d/%d", inf.account.AccountID%100, inf.account.AccountID)
		trans.updates[uidRef] = &RefUpdate{NewID: id}

		for _, e := range inf.extIDs {
			cfg := &config.Config{}
			cfg.SetOption("externalId", e.Identity, "accountId", strconv.Itoa(inf.account.AccountID))
			if e.EmailAddress != "" {
				cfg.SetOption("externalId", e.Identity, "email", e.EmailAddress)
			}

			id, err := gitutil.SaveConfig(repo.Storer, cfg)
			if err != nil {
				return err
			}

			newEntries = append(newEntries, object.TreeEntry{
				Name: fmt.Sprintf("%x", sha1.Sum([]byte(e.Identity))),
				Mode: filemode.Regular,
				Hash: id,
			})
		}
	}

	var prevExtIDTree object.Tree
	if extCommit != nil {
		tree, err := repo.TreeObject(extCommit.TreeHash)
		if err != nil {
			return err
		}
		prevExtIDTree = *tree
	}

	id, err := gitutil.PatchTree(repo.Storer, &prevExtIDTree, newEntries)
	if err != nil {
		return err
	}

	newExtCommit := &object.Commit{
		Author:    s,
		Committer: s,
		TreeHash:  id,
		Message:   "update external IDs",
	}
	if extCommit != nil {
		newExtCommit.ParentHashes = []plumbing.Hash{extCommit.Hash}
	}
	id, err = gitutil.SaveCommit(repo.Storer, newExtCommit)
	if err != nil {
		return err
	}

	trans.updates[string(extRefName)] = &RefUpdate{NewID: id}

	return UpdateRepo(repo.Storer, trans)
}

func main() {
	url := flag.String("url", "http://localhost:8080/", "")
	repoDir := flag.String("repo", "", "all-users repo")
	flag.Parse()
	if *repoDir == "" {
		log.Fatal("must specify --repo")
	}

	if flag.NArg() == 0 {
		log.Fatal("must specify 1 or more account IDs.")
	}

	repo, err := git.PlainOpen(*repoDir)
	if err != nil {
		log.Fatal(err)
	}

	client, err := gerrit.NewClient(*url, nil)
	if err != nil {
		log.Fatal(err)
	}

	// must have view_database privilege.
	client.Authentication.SetBasicAuth("admin", "XqDG4yB3JMAIVnrp7BJDC3Q3luc2GIk+UBYUqHH2GQ")

	// Get all public projects
	var infos []*AccountInfo
	for _, id := range flag.Args() {
		val, err := getAccountDetails(client, id)
		if val == nil {
			continue
		}
		if err != nil {
			log.Fatal(err)
		}
		infos = append(infos, val)
	}

	if len(infos) == 0 {
		log.Println("nothing to do.")
		os.Exit(0)
	}
	if err := saveAccountDetails(infos, repo); err != nil {
		log.Fatal(err)
	}
}
