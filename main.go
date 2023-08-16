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
	"context"
	"crypto/sha1"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/hanwen/allusersync/gitutil"
	gerrit "github.com/hanwen/go-gerrit"
	"golang.org/x/time/rate"
)

type AccountInfo struct {
	account gerrit.AccountDetailInfo
	extIDs  []gerrit.AccountExternalIdInfo
}

func getAccountDetails(lim *rate.Limiter, cl *gerrit.Client, id string) (*AccountInfo, error) {
	lim.Wait(context.Background())
	details, reply, err := cl.Accounts.GetAccountDetails(id)

	if reply != nil && reply.StatusCode == 404 {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}
	lim.Wait(context.Background())
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
	NewID plumbing.Hash
}

type RefTransaction struct {
	updates map[plumbing.ReferenceName]*RefUpdate
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
	extRefName := plumbing.ReferenceName("refs/meta/external-ids")
	extRef, err := repo.Reference(extRefName, true)
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
		updates: map[plumbing.ReferenceName]*RefUpdate{},
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

		uidRefName := plumbing.ReferenceName(fmt.Sprintf("refs/users/%02d/%d", inf.account.AccountID%100, inf.account.AccountID))
		uidRef, err := repo.Reference(uidRefName, true)
		var oldUserCommit *object.Commit
		if err == plumbing.ErrReferenceNotFound {
			err = nil
		}
		if err != nil {
			return err
		}
		if uidRef != nil {
			oldUserCommit, err = repo.CommitObject(uidRef.Hash())
			if err != nil {
				return err
			}
		}

		// TODO - could work registration date into Author/committer timestamp
		uidCommit := &object.Commit{
			Author:    s,
			Committer: s,
			Message:   "update account",
			TreeHash:  id,
		}

		if oldUserCommit != nil {
			if oldUserCommit.TreeHash == uidCommit.TreeHash {
				continue
			}
			uidCommit.ParentHashes = []plumbing.Hash{oldUserCommit.Hash}

			// TODO - work out differences, and schedule old external IDs for deletion.
		}

		id, err = gitutil.SaveCommit(repo.Storer, uidCommit)
		if err != nil {
			return err
		}

		trans.updates[uidRefName] = &RefUpdate{NewID: id}

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

			// TODO - support sharded notemap?
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

	if extCommit == nil || extCommit.TreeHash != newExtCommit.TreeHash {
		trans.updates[extRefName] = &RefUpdate{NewID: id}
	}

	return UpdateRepo(repo.Storer, trans)
}

func main() {
	url := flag.String("url", "http://localhost:8080/", "")
	repoDir := flag.String("repo", "", "all-users repo")

	basicAuth := flag.String("basic", "", "USER:PASSWORD for basic auth.")
	cookieAuth := flag.String("cookie", "", "value for the 'o' auth cookie. Use for googlesource.com")
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

	if *basicAuth != "" {
		fields := strings.Split(*basicAuth, ":")
		client.Authentication.SetBasicAuth(fields[0], fields[1])
	} else if *cookieAuth != "" {
		client.Authentication.SetCookieAuth("o", *cookieAuth)
	}

	caps, _, err := client.Accounts.ListAccountCapabilities("self", nil)
	if err != nil {
		log.Fatal(err)
	}

	if !caps.AccessDatabase {
		log.Fatal("need accessDatabase capability.")
	}

	var infos []*AccountInfo

	// googlesource.com caps at 8 QPS for logged-in users.
	lim := rate.NewLimiter(8, 4)

	// TODO - use account query to fetch AccountInfo data in bulk,
	// so we can get account details for many IDs in one call.
	// Right now, we have to probe all integer account IDs.
	for _, id := range flag.Args() {
		val, err := getAccountDetails(lim, client, id)
		if val == nil {
			continue
		}
		if err != nil {
			log.Fatal(err)
		}
		infos = append(infos, val)
		if len(infos)%100 == 0 {
			fmt.Printf("%s ... ", id)
		}
	}

	if len(infos) == 0 {
		log.Println("nothing to do.")
		os.Exit(0)
	}
	if err := saveAccountDetails(infos, repo); err != nil {
		log.Fatal(err)
	}
}
