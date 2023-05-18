/*
 * Flow Emulator
 *
 * Copyright 2019 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package remote

import (
	"context"
	"fmt"

	"github.com/onflow/flow-archive/api/archive"
	"github.com/onflow/flow-archive/codec/zbor"
	exeState "github.com/onflow/flow-go/engine/execution/state"
	"github.com/onflow/flow-go/fvm/state"
	"github.com/onflow/flow-go/fvm/storage/snapshot"
	"github.com/onflow/flow-go/ledger/common/pathfinder"
	"github.com/onflow/flow-go/ledger/complete"
	flowgo "github.com/onflow/flow-go/model/flow"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/onflow/flow-emulator/storage"
	"github.com/onflow/flow-emulator/storage/sqlite"
)

type Store struct {
	*sqlite.Store
	client archive.APIClient
}

func New(chainID flowgo.ChainID) (*Store, error) {
	archiveHosts := map[flowgo.ChainID]string{
		flowgo.Mainnet: "archive.mainnet.nodes.onflow.org:9000",
		flowgo.Testnet: "archive.testnet.nodes.onflow.org:9000",
	}

	host, ok := archiveHosts[chainID]
	if !ok {
		return nil, fmt.Errorf("chain %s not supported with remote store", chainID.String())
	}

	conn, err := grpc.Dial(
		host,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("could not connect to archive node: %w", err)
	}

	memorySql, err := sqlite.New(sqlite.InMemory)
	if err != nil {
		return nil, err
	}

	store := &Store{
		client: archive.NewAPIClient(conn),
		Store:  memorySql,
	}

	store.DataGetter = store
	store.DataSetter = store
	store.KeyGenerator = &storage.DefaultKeyGenerator{}

	return store, nil
}

func (s *Store) BlockByID(ctx context.Context, blockID flowgo.Identifier) (*flowgo.Block, error) {
	// todo use local storage first as a cache

	heightRes, err := s.client.GetHeightForBlock(ctx, &archive.GetHeightForBlockRequest{BlockID: blockID[:]})
	if err != nil {
		return nil, err
	}

	return s.BlockByHeight(ctx, heightRes.Height)
}

func (s *Store) LatestBlock(ctx context.Context) (flowgo.Block, error) {
	heightRes, err := s.client.GetLast(ctx, &archive.GetLastRequest{})
	if err != nil {
		return flowgo.Block{}, err
	}

	block, err := s.BlockByHeight(ctx, heightRes.Height)
	if err != nil {
		return flowgo.Block{}, err
	}

	return *block, nil
}

func (s *Store) BlockByHeight(ctx context.Context, height uint64) (*flowgo.Block, error) {
	// todo use local storage first as a cache

	blockRes, err := s.client.GetHeader(ctx, &archive.GetHeaderRequest{Height: height})
	if err != nil {
		return nil, err
	}

	var header flowgo.Header
	err = zbor.NewCodec().Unmarshal(blockRes.Data, &header)
	if err != nil {
		return nil, err
	}

	payload := flowgo.EmptyPayload()
	return &flowgo.Block{
		Payload: &payload,
		Header:  &header,
	}, nil
}

func (s *Store) LedgerByHeight(
	ctx context.Context,
	blockHeight uint64,
) state.StorageSnapshot {
	_ = s.SetBlockHeight(blockHeight)

	return snapshot.NewReadFuncStorageSnapshot(func(id flowgo.RegisterID) (flowgo.RegisterValue, error) {
		// first try to see if we have local stored ledger
		value, err := s.DefaultStore.GetBytesAtVersion(ctx, "ledger", []byte(id.String()), blockHeight)
		if !errors.Is(err, storage.ErrNotFound) {
			if err != nil {
				return nil, err
			}
			return value, nil
		}

		ledgerKey := exeState.RegisterIDToKey(flowgo.RegisterID{Key: id.Key, Owner: id.Owner})
		ledgerPath, err := pathfinder.KeyToPath(ledgerKey, complete.DefaultPathFinderVersion)
		if err != nil {
			return nil, err
		}

		response, err := s.client.GetRegisterValues(ctx, &archive.GetRegisterValuesRequest{
			Height: blockHeight,
			Paths:  [][]byte{ledgerPath[:]},
		})
		if err != nil {
			return nil, err
		}

		if len(response.Values) == 0 {
			return nil, fmt.Errorf("not found value for register id %s", id.String())
		}

		return response.Values[0], nil
	})
}
