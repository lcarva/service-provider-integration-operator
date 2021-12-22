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

package oauthstate

import (
	"github.com/go-jose/go-jose/v3"
)

// Codec is in charge of encoding and decoding the state passed through the OAuth flow as the state query parameter.
type Codec struct {
	signer        jose.Signer
	signingSecret []byte
}

// NewCodec creates a new codec using the secret used for signing the JWT tokens that represent the state in the
// query parameters. The signing is used to make it harder to forge malicious OAuth flow requests. We don't need to
// encrypt the state strings, because they don't contain any information that would not be obtainable from the requests
// initiating the OAuth flow.
func NewCodec(signingSecret []byte) (Codec, error) {
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.HS256,
		Key:       signingSecret,
	}, (&jose.SignerOptions{}).WithType("SPI"))
	if err != nil {
		return Codec{}, err
	}

	return Codec{
		signer:        signer,
		signingSecret: signingSecret,
	}, nil
}