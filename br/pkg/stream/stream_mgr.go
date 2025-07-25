// Copyright 2021 PingCAP, Inc.
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

package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/encryptionpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/encryption"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/metadef"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/util"
	filter "github.com/pingcap/tidb/pkg/util/table-filter"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	streamBackupMetaPrefix = "v1/backupmeta"

	streamBackupGlobalCheckpointPrefix = "v1/global_checkpoint"
)

// metaPattern is a regular expression used to match backup metadata filenames.
// The expected filename format is:
//
//	{flushTs}-{minDefaultTs}-{minTs}-{maxTs}.meta
//
// where each part is a hexadecimal string (0-9, a-f, A-F).
// Example:
//
//	0000000000000001-0000000000003039-065CCFF1D8AC0000-065CCFF1D8AC0006.meta
//
// The pattern captures all four parts as separate groups.
// Leading zeros are necessary for the pattern to match.
var metaPattern = regexp.MustCompile(`^([0-9a-fA-F]{16})-([0-9a-fA-F]{16})-([0-9a-fA-F]{16})-([0-9a-fA-F]{16})$`)

func GetStreamBackupMetaPrefix() string {
	return streamBackupMetaPrefix
}

func GetStreamBackupGlobalCheckpointPrefix() string {
	return streamBackupGlobalCheckpointPrefix
}

// appendTableObserveRanges builds key ranges corresponding to `tblIDS`.
func appendTableObserveRanges(tblIDs []int64) []kv.KeyRange {
	krs := make([]kv.KeyRange, 0, len(tblIDs))
	for _, tid := range tblIDs {
		startKey := tablecodec.GenTableRecordPrefix(tid)
		endKey := startKey.PrefixNext()
		krs = append(krs, kv.KeyRange{StartKey: startKey, EndKey: endKey})
	}
	return krs
}

// buildObserveTableRange builds key ranges to observe data KV that belongs to `table`.
func buildObserveTableRange(table *model.TableInfo) []kv.KeyRange {
	pis := table.GetPartitionInfo()
	if pis == nil {
		// Short path, no partition.
		return appendTableObserveRanges([]int64{table.ID})
	}

	tblIDs := make([]int64, 0, len(pis.Definitions))
	// whether we shoud append tbl.ID into tblIDS ?
	for _, def := range pis.Definitions {
		tblIDs = append(tblIDs, def.ID)
	}
	return appendTableObserveRanges(tblIDs)
}

// buildObserveTableRanges builds key ranges to observe table kv-events.
func buildObserveTableRanges(
	storage kv.Storage,
	tableFilter filter.Filter,
	backupTS uint64,
) ([]kv.KeyRange, error) {
	snapshot := storage.GetSnapshot(kv.NewVersion(backupTS))
	m := meta.NewReader(snapshot)

	dbs, err := m.ListDatabases()
	if err != nil {
		return nil, errors.Trace(err)
	}

	ranges := make([]kv.KeyRange, 0, len(dbs)+1)
	for _, dbInfo := range dbs {
		if !tableFilter.MatchSchema(dbInfo.Name.O) || metadef.IsMemDB(dbInfo.Name.L) {
			continue
		}

		if err := m.IterTables(dbInfo.ID, func(tableInfo *model.TableInfo) error {
			if !tableFilter.MatchTable(dbInfo.Name.O, tableInfo.Name.O) {
				// Skip tables other than the given table.
				return nil
			}
			log.Info("start to observe the table", zap.Stringer("db", dbInfo.Name), zap.Stringer("table", tableInfo.Name))

			tableRanges := buildObserveTableRange(tableInfo)
			ranges = append(ranges, tableRanges...)
			return nil
		}); err != nil {
			return nil, errors.Trace(err)
		}
	}

	return ranges, nil
}

// buildObserverAllRange build key range to observe all data kv-events.
func buildObserverAllRange() []kv.KeyRange {
	var startKey []byte
	startKey = append(startKey, tablecodec.TablePrefix()...)

	sk := kv.Key(startKey)
	ek := sk.PrefixNext()

	rgs := make([]kv.KeyRange, 0, 1)
	return append(rgs, kv.KeyRange{StartKey: sk, EndKey: ek})
}

// BuildObserveDataRanges builds key ranges to observe data KV.
func BuildObserveDataRanges(
	storage kv.Storage,
	filterStr []string,
	tableFilter filter.Filter,
	backupTS uint64,
) ([]kv.KeyRange, error) {
	if len(filterStr) == 1 && filterStr[0] == string("*.*") {
		return buildObserverAllRange(), nil
	}
	// TODO: currently it's a dead code, the iterator metakvs can be optimized
	//  to marshal only necessary fields.
	return buildObserveTableRanges(storage, tableFilter, backupTS)
}

// BuildObserveMetaRange specifies build key ranges to observe meta KV(contains all of metas)
func BuildObserveMetaRange() *kv.KeyRange {
	var startKey []byte
	startKey = append(startKey, tablecodec.MetaPrefix()...)
	sk := kv.Key(startKey)
	ek := sk.PrefixNext()

	return &kv.KeyRange{StartKey: sk, EndKey: ek}
}

type ContentRef struct {
	init_ref int
	ref      int
	data     []byte
}

// MetadataHelper make restore/truncate compatible with metadataV1 and metadataV2.
type MetadataHelper struct {
	cache             map[string]*ContentRef
	decoder           *zstd.Decoder
	encryptionManager *encryption.Manager
}

type MetadataHelperOption func(*MetadataHelper)

func WithEncryptionManager(manager *encryption.Manager) MetadataHelperOption {
	return func(mh *MetadataHelper) {
		mh.encryptionManager = manager
	}
}

func NewMetadataHelper(opts ...MetadataHelperOption) *MetadataHelper {
	decoder, _ := zstd.NewReader(nil)
	helper := &MetadataHelper{
		cache:   make(map[string]*ContentRef),
		decoder: decoder,
	}

	for _, opt := range opts {
		opt(helper)
	}

	return helper
}

func (m *MetadataHelper) InitCacheEntry(path string, ref int) {
	if ref <= 0 {
		return
	}
	m.cache[path] = &ContentRef{
		init_ref: ref,
		ref:      ref,
		data:     nil,
	}
}

func (m *MetadataHelper) decodeCompressedData(data []byte, compressionType backuppb.CompressionType) ([]byte, error) {
	switch compressionType {
	case backuppb.CompressionType_UNKNOWN:
		return data, nil
	case backuppb.CompressionType_ZSTD:
		return m.decoder.DecodeAll(data, nil)
	}
	return nil, errors.Errorf(
		"failed to decode compressed data: compression type is unimplemented. type id is %d", compressionType)
}

func (m *MetadataHelper) verifyChecksumAndDecryptIfNeeded(ctx context.Context, data []byte,
	encryptionInfo *encryptionpb.FileEncryptionInfo) ([]byte, error) {
	// no need to decrypt
	if encryptionInfo == nil {
		return data, nil
	}

	if m.encryptionManager == nil {
		return nil, errors.New("need to decrypt data but encryption manager not set")
	}

	// Verify checksum before decryption
	if encryptionInfo.Checksum != nil {
		actualChecksum := sha256.Sum256(data)
		expectedChecksumHex := hex.EncodeToString(encryptionInfo.Checksum)
		actualChecksumHex := hex.EncodeToString(actualChecksum[:])
		if expectedChecksumHex != actualChecksumHex {
			return nil, errors.Errorf("checksum mismatch before decryption, expected %s, actual %s",
				expectedChecksumHex, actualChecksumHex)
		}
	}

	decryptedContent, err := m.encryptionManager.Decrypt(ctx, data, encryptionInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return decryptedContent, nil
}

func (m *MetadataHelper) ReadFile(
	ctx context.Context,
	path string,
	offset uint64,
	length uint64,
	compressionType backuppb.CompressionType,
	storage storage.ExternalStorage,
	encryptionInfo *encryptionpb.FileEncryptionInfo,
) ([]byte, error) {
	var err error
	cref, exist := m.cache[path]
	if !exist {
		// Only files from metaV2 are cached,
		// so the file should be from metaV1.
		if offset > 0 || length > 0 {
			// But the file is from metaV2.
			return nil, errors.Errorf("the cache entry is uninitialized")
		}
		data, err := storage.ReadFile(ctx, path)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// decrypt if needed
		decryptedData, err := m.verifyChecksumAndDecryptIfNeeded(ctx, data, encryptionInfo)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return m.decodeCompressedData(decryptedData, compressionType)
	}

	cref.ref -= 1

	if len(cref.data) == 0 {
		cref.data, err = storage.ReadFile(ctx, path)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	// decrypt if needed
	decryptedData, err := m.verifyChecksumAndDecryptIfNeeded(ctx, cref.data[offset:offset+length], encryptionInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}
	buf, err := m.decodeCompressedData(decryptedData, compressionType)

	if cref.ref <= 0 {
		// need reset reference information.
		cref.data = nil
		cref.ref = cref.init_ref
	}

	return buf, errors.Trace(err)
}

func (*MetadataHelper) ParseToMetadata(rawMetaData []byte) (*backuppb.Metadata, error) {
	meta := &backuppb.Metadata{}
	err := meta.Unmarshal(rawMetaData)
	if meta.MetaVersion == backuppb.MetaVersion_V1 {
		group := &backuppb.DataFileGroup{
			// For MetaDataV2, file's path is stored in it.
			Path: "",
			// In fact, each file in MetaDataV1 can be regard
			// as a file group in MetaDataV2. But for simplicity,
			// the files in MetaDataV1 are considered as a group.
			DataFilesInfo: meta.Files,
			// Other fields are Unused.
		}
		meta.FileGroups = []*backuppb.DataFileGroup{group}
	}
	return meta, errors.Trace(err)
}

// Only for deleting, after MetadataV1 is deprecated, we can remove it.
// Hard means convert to MetaDataV2 deeply.
func (*MetadataHelper) ParseToMetadataHard(rawMetaData []byte) (*backuppb.Metadata, error) {
	meta := &backuppb.Metadata{}
	err := meta.Unmarshal(rawMetaData)
	if meta.MetaVersion == backuppb.MetaVersion_V1 {
		groups := make([]*backuppb.DataFileGroup, 0, len(meta.Files))
		for _, d := range meta.Files {
			groups = append(groups, &backuppb.DataFileGroup{
				// For MetaDataV2, file's path is stored in it.
				Path: d.Path,
				// Each file in MetaDataV1 can be regard
				// as a file group in MetaDataV2.
				DataFilesInfo: []*backuppb.DataFileInfo{d},
				MaxTs:         d.MaxTs,
				MinTs:         d.MinTs,
				MinResolvedTs: d.ResolvedTs,
				// File from MetaVersion_V1 isn't compressed.
				Length: d.Length,
				// Other fields are Unused.
			})
		}
		meta.FileGroups = groups
	}
	return meta, errors.Trace(err)
}

// For truncate command. Marshal metadata to reupload to external storage.
// The metadata must be unmarshal by `ParseToMetadataHard`
func (*MetadataHelper) Marshal(meta *backuppb.Metadata) ([]byte, error) {
	// the field `Files` isn't modified.
	if meta.MetaVersion == backuppb.MetaVersion_V1 {
		if len(meta.FileGroups) != len(meta.Files) {
			// some files are deleted
			files := make([]*backuppb.DataFileInfo, 0, len(meta.FileGroups))
			for _, g := range meta.FileGroups {
				files = append(files, g.DataFilesInfo...)
			}
			meta.Files = files
		}
		meta.FileGroups = nil
	}
	return meta.Marshal()
}

func (m *MetadataHelper) Close() {
	if m.decoder != nil {
		m.decoder.Close()
	}
	if m.encryptionManager != nil {
		m.encryptionManager.Close()
	}
}

func FilterPathByTs(path string, left, right uint64) string {
	filename := strings.TrimSuffix(path, ".meta")
	filename = filename[strings.LastIndex(filename, "/")+1:]

	if metaPattern.MatchString(filename) {
		matches := metaPattern.FindStringSubmatch(filename)
		if len(matches) < 5 {
			log.Warn("invalid meta file name format", zap.String("file", path))
			// consider compatible with future file path change
			return path
		}

		flushTs, _ := strconv.ParseUint(matches[1], 16, 64)
		minDefaultTs, _ := strconv.ParseUint(matches[2], 16, 64)
		minTs, _ := strconv.ParseUint(matches[3], 16, 64)
		maxTs, _ := strconv.ParseUint(matches[4], 16, 64)

		if minDefaultTs == 0 || minDefaultTs > minTs {
			log.Warn("minDefaultTs is not correct, fallback to minTs",
				zap.String("file", path),
				zap.Uint64("flushTs", flushTs),
				zap.Uint64("minTs", minTs),
				zap.Uint64("minDefaultTs", minDefaultTs),
			)
			minDefaultTs = minTs
		}

		if right < minDefaultTs || maxTs < left {
			return ""
		}
	}

	// keep consistency with old behaviour
	return path
}

// FastUnmarshalMetaData used a 128 worker pool to speed up
// read metadata content from external_storage.
func FastUnmarshalMetaData(
	ctx context.Context,
	s storage.ExternalStorage,
	startTS uint64,
	endTS uint64,
	metaDataWorkerPoolSize uint,
	fn func(path string, rawMetaData []byte) error,
) error {
	log.Info("use workers to speed up reading metadata files", zap.Uint("workers", metaDataWorkerPoolSize))
	pool := util.NewWorkerPool(metaDataWorkerPoolSize, "metadata")
	eg, ectx := errgroup.WithContext(ctx)
	opt := &storage.WalkOption{SubDir: GetStreamBackupMetaPrefix()}
	err := s.WalkDir(ectx, opt, func(path string, size int64) error {
		if !strings.HasSuffix(path, metaSuffix) {
			return nil
		}
		readPath := FilterPathByTs(path, startTS, endTS)
		if len(readPath) == 0 {
			log.Info("skip download meta file out of range",
				zap.String("file", path),
				zap.Uint64("startTs", startTS),
				zap.Uint64("endTs", endTS),
			)
			return nil
		}
		pool.ApplyOnErrorGroup(eg, func() error {
			b, err := s.ReadFile(ectx, readPath)
			if err != nil {
				log.Error("failed to read file", zap.String("file", readPath))
				return errors.Annotatef(err, "during reading meta file %s from storage", readPath)
			}

			return fn(readPath, b)
		})
		return nil
	})
	if err != nil {
		readErr := eg.Wait()
		if readErr != nil {
			return errors.Annotatef(readErr, "scanning metadata meets error %s", err)
		}
		return errors.Annotate(err, "scanning metadata meets error")
	}
	return eg.Wait()
}
