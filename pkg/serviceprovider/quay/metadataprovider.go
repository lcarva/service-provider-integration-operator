//
// Copyright (c) 2021 Red Hat, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package quay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/redhat-appstudio/service-provider-integration-operator/api/v1beta1"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/serviceprovider"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/spi-shared/tokenstorage"
)

type metadataProvider struct {
	tokenStorage     tokenstorage.TokenStorage
	httpClient       *http.Client
	kubernetesClient client.Client
	ttl              time.Duration
}

var _ serviceprovider.MetadataProvider = (*metadataProvider)(nil)
var invalidRepoUrl = errors.New("invalid repository URL")

func (p metadataProvider) Fetch(ctx context.Context, token *api.SPIAccessToken) (*api.TokenMetadata, error) {
	lg := log.FromContext(ctx, "tokenName", token.Name, "tokenNamespace", token.Namespace)

	data, err := p.tokenStorage.Get(ctx, token)
	if err != nil {
		lg.Error(err, "failed to get the token metadata")
		return nil, fmt.Errorf("failed to get the token metadata: %w", err)
	}

	if data == nil {
		return nil, nil
	}

	// This method is called when we need to refresh (or obtain anew, after cache expiry) the metadata of the token.
	// Because we load all the state iteratively for Quay, this info is always empty when fresh.

	state := &TokenState{
		Repositories:  map[string]EntityRecord{},
		Organizations: map[string]EntityRecord{},
	}

	js, err := json.Marshal(state)
	if err != nil {
		lg.Error(err, "failed to serialize the token metadata, this should not happen")
		return nil, fmt.Errorf("failed to marshal the state to JSON: %w", err)
	}

	metadata := token.Status.TokenMetadata
	if metadata == nil {
		metadata = &api.TokenMetadata{}
		token.Status.TokenMetadata = metadata
	}

	if len(data.Username) > 0 {
		metadata.Username = data.Username
	} else {
		metadata.Username = OAuthTokenUserName
	}

	metadata.ServiceProviderState = js

	lg.Info("token metadata initialized")

	return metadata, nil
}

// RepositoryMetadata is the return value of the FetchRepo method. It represents the scopes that are granted for some
// token on a given repository and organization it belongs to.
type RepositoryMetadata struct {
	Repository   EntityRecord
	Organization EntityRecord
}

// FetchRepo is the iterative version of Fetch used internally in the Quay service provider. It takes care of both
// fetching and caching of the data on per-repository basis.
func (p metadataProvider) FetchRepo(ctx context.Context, repoUrl string, token *api.SPIAccessToken) (metadata RepositoryMetadata, err error) {
	lg := log.FromContext(ctx, "repo", repoUrl, "tokenName", token.Name, "tokenNamespace", token.Namespace)

	lg.Info("fetching repository metadata")

	if token.Status.TokenMetadata == nil {
		lg.Info("no metadata on the token object, bailing")
		return
	}

	quayState := TokenState{}
	if err = json.Unmarshal(token.Status.TokenMetadata.ServiceProviderState, &quayState); err != nil {
		lg.Error(err, "failed to unmarshal quay token state")
		err = fmt.Errorf("failed to unmarshal the token state: %w", err)
		return
	}
	if quayState.Repositories == nil || quayState.Organizations == nil {
		lg.Info("Detected quay token state with empty Repositories or Organizations")
		if quayState.Repositories == nil {
			quayState.Repositories = make(map[string]EntityRecord)
		}

		if quayState.Organizations == nil {
			quayState.Organizations = make(map[string]EntityRecord)
		}
	}

	var tokenData *api.Token

	tokenData, err = p.tokenStorage.Get(ctx, token)
	if err != nil {
		lg.Error(err, "failed to get token data")
		err = fmt.Errorf("failed to get the token data from storage: %w", err)
		return
	}
	if tokenData == nil {
		lg.Info("no token data found")
		return
	}

	orgOrUser, repo, _ := splitToOrganizationAndRepositoryAndVersion(repoUrl)
	if orgOrUser == "" || repo == "" {
		lg.Error(err, "failed to parse the repository URL")
		err = fmt.Errorf("%w %s", invalidRepoUrl, repoUrl)
		return
	}

	// enable the lazy one-time login to docker in the subsequent calls
	var loginToken *LoginTokenInfo
	getLoginTokenInfo := func() (LoginTokenInfo, error) {
		if loginToken != nil {
			return *loginToken, nil
		}

		username, password := getUsernameAndPasswordFromTokenData(tokenData)
		var tkn string
		tkn, err = DockerLogin(log.IntoContext(ctx, lg), p.httpClient, orgOrUser+"/"+repo, username, password)
		if err != nil {
			lg.Error(err, "failed to perform docker login")
			return LoginTokenInfo{}, fmt.Errorf("fetch failed due to docker login error: %w", err)
		}

		// empty token means the credentials are no longer valid, which is not an error in and of itself
		if tkn == "" {
			return LoginTokenInfo{}, nil
		}

		info, err := AnalyzeLoginToken(tkn)
		if err != nil {
			lg.Error(err, "failed to analyze the docker login token")
			return LoginTokenInfo{}, fmt.Errorf("failed to analyze the docker login token: %w", err)
		}

		lg.Info("used docker login to detect rw perms successfully", "repo:read",
			info.Repositories[repo].Pullable, "repo:write", info.Repositories[repo].Pushable)

		loginToken = &info

		return info, nil
	}

	var orgChanged, repoChanged bool
	var orgRecord, repoRecord EntityRecord

	orgRecord, orgChanged, err = p.getEntityRecord(log.IntoContext(ctx, lg.WithValues("entityType", "organization")), tokenData, orgOrUser, quayState.Organizations, getLoginTokenInfo, fetchOrganizationRecord)
	if err != nil {
		lg.Error(err, "failed to read the organization metadata")
		err = fmt.Errorf("failed to read organization metadata: %w", err)
		return
	}

	repoRecord, repoChanged, err = p.getEntityRecord(log.IntoContext(ctx, lg.WithValues("entityType", "repository")), tokenData, orgOrUser+"/"+repo, quayState.Repositories, getLoginTokenInfo, fetchRepositoryRecord)
	if err != nil {
		lg.Error(err, "failed to read the repository metadata")
		err = fmt.Errorf("failed to read repository metadata: %w", err)
		return
	}

	if orgChanged || repoChanged {
		if err = p.persistTokenState(ctx, token, &quayState); err != nil {
			lg.Error(err, "failed to persist the metadata changes")
			return
		}
	}

	return RepositoryMetadata{
		Repository:   repoRecord,
		Organization: orgRecord,
	}, nil
}

// helper types for getEntityRecord parameters
type loginInfoFn func() (LoginTokenInfo, error)
type fetchEntityRecordFn func(ctx context.Context, cl *http.Client, repoUrl string, tokenData *api.Token, info LoginTokenInfo) (*EntityRecord, error)

// getEntityRecord is a method for getting the different types of repository scopes in a uniform fashion. If needed,
// it updates the provided cache with the new data and uses the loginInfoFn to fetch the docker login info and fetchFn
// for actually fetching the EntityRecord (only when needed - i.e. if the data for given key is not in the cache or
// is expired).
func (p metadataProvider) getEntityRecord(ctx context.Context, tokenData *api.Token, key string, cache map[string]EntityRecord, loginInfoFn loginInfoFn, fetchFn fetchEntityRecordFn) (rec EntityRecord, changed bool, err error) {
	rec, present := cache[key]

	lg := log.FromContext(ctx)

	if !present || time.Now().After(time.Unix(rec.LastRefreshTime, 0).Add(p.ttl)) {
		lg.Info("entity metadata stale, reloading")

		var repoRec *EntityRecord

		var loginInfo LoginTokenInfo
		loginInfo, err = loginInfoFn()
		if err != nil {
			lg.Error(err, "failed to get the docker login info")
			return
		}

		repoRec, err = fetchFn(ctx, p.httpClient, key, tokenData, loginInfo)
		if err != nil {
			lg.Error(err, "failed to fetch the metadata entity")
			return
		}

		if repoRec == nil {
			repoRec = &EntityRecord{}
		}

		repoRec.LastRefreshTime = time.Now().Unix()

		cache[key] = *repoRec
		rec = *repoRec
		changed = true
		lg.Info("metadata entity fetched successfully")
	} else {
		lg.Info("requested metadata entity still valid")
	}

	return
}

// persistTokenState persists the provided tokenState in the token object's status and saves it to the cluster.
func (p metadataProvider) persistTokenState(ctx context.Context, token *api.SPIAccessToken, tokenState *TokenState) error {
	lg := log.FromContext(ctx)

	data, err := json.Marshal(tokenState)
	if err != nil {
		lg.Error(err, "failed to serialize the metadata")
		err = fmt.Errorf("failed to serialize the metadata: %w", err)
		return err
	}

	token.Status.TokenMetadata.ServiceProviderState = data

	if err = p.kubernetesClient.Status().Update(ctx, token); err != nil {
		return fmt.Errorf("failed to persist the token metadata: %w", err)
	}

	return nil
}
