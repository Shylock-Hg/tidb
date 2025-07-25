// Copyright 2024 PingCAP, Inc.
//
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

package importer_test

import (
	"context"
	"os"
	"testing"

	"github.com/pingcap/failpoint"
	tidb "github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/executor/importer"
	tidbkv "github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/lightning/backend/local"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	plannercore "github.com/pingcap/tidb/pkg/planner/core"
	"github.com/pingcap/tidb/pkg/planner/core/operator/physicalop"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/session"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/tikv"
)

type storeHelper struct {
	kvStore tidbkv.Storage
}

func (*storeHelper) GetTS(_ context.Context) (physical, logical int64, err error) {
	return 0, 0, nil
}

func (s *storeHelper) GetTiKVCodec() tikv.Codec {
	return s.kvStore.GetCodec()
}

var _ local.StoreHelper = (*storeHelper)(nil)

func checkImportDirEmpty(t *testing.T) {
	tidbCfg := tidb.GetGlobalConfig()
	importDir := importer.GetImportRootDir(tidbCfg)
	if _, err := os.Stat(importDir); err != nil {
		require.True(t, os.IsNotExist(err), importDir)
	} else {
		entries, err := os.ReadDir(importDir)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestImportFromSelectCleanup(t *testing.T) {
	ctx := context.Background()
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	tidbCfg := tidb.GetGlobalConfig()
	tidbCfg.TempDir = t.TempDir()
	checkImportDirEmpty(t)

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/executor/importer/mockImportFromSelectErr", `return(true)`))
	t.Cleanup(func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/executor/importer/mockImportFromSelectErr"))
	})

	tk.MustExec("use test")
	tk.MustExec("create table t(a int)")
	do, err := session.GetDomain(store)
	require.NoError(t, err)
	dbInfo, ok := do.InfoSchema().SchemaByName(ast.NewCIStr("test"))
	require.True(t, ok)
	table, err := do.InfoSchema().TableByName(context.Background(), ast.NewCIStr("test"), ast.NewCIStr("t"))
	require.NoError(t, err)
	plan, err := importer.NewImportPlan(ctx, tk.Session(), plannercore.ImportInto{
		Table: &resolve.TableNameW{
			TableName: &ast.TableName{
				Name: ast.NewCIStr("t"),
			},
			DBInfo: &model.DBInfo{
				Name: ast.NewCIStr("test"),
				ID:   dbInfo.ID,
			},
		},
		SelectPlan: &physicalop.PhysicalSelection{},
	}.Init(tk.Session().GetPlanCtx()), table)
	require.NoError(t, err)
	controller, err := importer.NewLoadDataController(plan, table, &importer.ASTArgs{})
	require.NoError(t, err)
	ti, err := importer.NewTableImporterForTest(
		ctx,
		controller,
		"11",
		&storeHelper{kvStore: store},
	)
	require.NoError(t, err)
	ch := make(chan importer.QueryChunk)
	ti.SetSelectedChunkCh(ch)
	var wg util.WaitGroupWrapper
	wg.Run(func() {
		defer close(ch)
		fields := make([]*types.FieldType, 0, 3)
		fields = append(fields, types.NewFieldType(mysql.TypeLong))
		chk := chunk.New(fields, 2, 2)
		chk.AppendInt64(0, int64(1))
		chk.AppendInt64(0, int64(2))
		ch <- importer.QueryChunk{
			Fields:      fields,
			Chk:         chk,
			RowIDOffset: 0,
		}
		chk = chunk.New(fields, 1, 1)
		chk.AppendInt64(0, int64(3))
		ch <- importer.QueryChunk{
			Fields:      fields,
			Chk:         chk,
			RowIDOffset: 2,
		}
	})
	_, err = ti.ImportSelectedRows(ctx, tk.Session())
	require.ErrorContains(t, err, "mock import from select error")
	wg.Wait()
	ti.Backend().CloseEngineMgr()
	checkImportDirEmpty(t)
}
