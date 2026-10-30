package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

type testMediaConnection struct {
	tg.Invoker
	closed bool
}

func (c *testMediaConnection) Close() error { c.closed = true; return nil }

type testMediaDCs struct {
	connections map[int]*testMediaConnection
	visited     []int
	cdn         *testMediaConnection
	cdnVisited  int
}

func (d *testMediaDCs) DC(_ context.Context, dc int, _ int64) (gotd.CloseInvoker, error) {
	d.visited = append(d.visited, dc)
	return d.connections[dc], nil
}

func (d *testMediaDCs) CDN(_ context.Context, dc int, _ int64) (gotd.CloseInvoker, error) {
	if d.cdn == nil {
		return nil, errors.New("unexpected CDN connection")
	}
	d.cdnVisited = dc
	return d.cdn, nil
}

func TestDownloadMediaFollowsDataCenterMigration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	first := tgmock.NewRequire(t)
	first.Expect().ThenRPCErr(tgerr.New(303, "FILE_MIGRATE_2"))
	second := tgmock.NewRequire(t)
	second.Expect().ThenResult(&tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: []byte("synthetic")})
	dcs := &testMediaDCs{connections: map[int]*testMediaConnection{1: {Invoker: first}, 2: {Invoker: second}}}
	ref := &MediaReference{DCID: 1, Size: 9, Location: &tg.InputDocumentFileLocation{ID: 123}}
	file, err := downloadMediaFromDC(context.Background(), dcs, ref, secureTempDir(t), 20, rate.NewLimiter(rate.Inf, 1))
	require.NoError(err)
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	data, err := io.ReadAll(file)
	require.NoError(err)
	assert.Equal("synthetic", string(data))
	assert.Equal([]int{1, 2}, dcs.visited)
	assert.True(dcs.connections[1].closed)
	assert.True(dcs.connections[2].closed)
}

func TestDownloadMediaStopsRepeatedDataCenterMigration(t *testing.T) {
	first := tgmock.NewRequire(t)
	first.Expect().ThenRPCErr(tgerr.New(303, "FILE_MIGRATE_2"))
	second := tgmock.NewRequire(t)
	second.Expect().ThenRPCErr(tgerr.New(303, "FILE_MIGRATE_1"))
	dcs := &testMediaDCs{connections: map[int]*testMediaConnection{1: {Invoker: first}, 2: {Invoker: second}}}
	ref := &MediaReference{DCID: 1, Size: 9, Location: &tg.InputDocumentFileLocation{ID: 123}}
	file, err := downloadMediaFromDC(context.Background(), dcs, ref, secureTempDir(t), 20, rate.NewLimiter(rate.Inf, 1))
	require.ErrorContains(t, err, "repeated")
	assert.Nil(t, file)
	assert.Equal(t, []int{1, 2}, dcs.visited)
}

func TestDownloadMediaChecksSizeBeforeOpeningDataCenter(t *testing.T) {
	dcs := &testMediaDCs{}
	ref := &MediaReference{DCID: 1, Size: 21, Location: &tg.InputDocumentFileLocation{ID: 123}}
	file, err := downloadMediaFromDC(context.Background(), dcs, ref, secureTempDir(t), 20, rate.NewLimiter(rate.Inf, 1))
	require.ErrorIs(t, err, ErrMediaTooLarge)
	assert.Nil(t, file)
	assert.Empty(t, dcs.visited)
}

func TestDownloadMediaFollowsVerifiedCDNRedirect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	data := []byte("synthetic")
	key, iv := make([]byte, 32), make([]byte, aes.BlockSize)
	block, err := aes.NewCipher(key)
	require.NoError(err)
	encrypted := make([]byte, len(data))
	cipher.NewCTR(block, iv).XORKeyStream(encrypted, data)
	hash := sha256.Sum256(data)
	master := tgmock.NewRequire(t)
	master.Expect().ThenResult(&tg.UploadFileCDNRedirect{DCID: 3, FileToken: []byte("token"), EncryptionKey: key, EncryptionIv: iv, FileHashes: []tg.FileHash{{Offset: 0, Limit: len(data), Hash: hash[:]}}})
	cdn := tgmock.NewRequire(t)
	cdn.Expect().ThenResult(&tg.UploadCDNFile{Bytes: encrypted})
	dcs := &testMediaDCs{connections: map[int]*testMediaConnection{1: {Invoker: master}}, cdn: &testMediaConnection{Invoker: cdn}}
	ref := &MediaReference{DCID: 1, Size: int64(len(data)), Location: &tg.InputDocumentFileLocation{ID: 123}}
	file, err := downloadMediaFromDC(context.Background(), dcs, ref, secureTempDir(t), 20, rate.NewLimiter(rate.Inf, 1))
	require.NoError(err)
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	got, err := io.ReadAll(file)
	require.NoError(err)
	assert.Equal(data, got)
	assert.Equal(3, dcs.cdnVisited)
	assert.True(dcs.cdn.closed)
}

func TestDownloadMediaReturnsCompleteTemporaryFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: []byte("synthetic")})
	builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
	file, err := DownloadMedia(context.Background(), builder, secureTempDir(t), 9, 10)
	require.NoError(err)
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	data, err := io.ReadAll(file)
	require.NoError(err)
	assert.Equal("synthetic", string(data))
	info, err := file.Stat()
	require.NoError(err)
	assert.Equal(os.FileMode(0600), info.Mode().Perm())
}

func TestDownloadMediaRejectsOversizedOrUnderreportedFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, declared := range []int64{11, -1} {
		dir := secureTempDir(t)
		mock := tgmock.NewRequire(t)
		if declared < 0 {
			mock.Expect().ThenResult(&tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: []byte("synthetic exceeds cap")})
		}
		builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
		file, err := DownloadMedia(context.Background(), builder, dir, declared, 10)
		require.ErrorIs(err, ErrMediaTooLarge)
		assert.Nil(file)
		entries, err := os.ReadDir(dir)
		require.NoError(err)
		assert.Empty(entries)
	}
}

func TestDownloadMediaCleansUpAfterRPCFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := secureTempDir(t)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenErr(errors.New("synthetic RPC failure"))
	builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
	file, err := DownloadMedia(context.Background(), builder, dir, -1, 10)
	require.Error(err)
	assert.Nil(file)
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(entries)
}

func TestDownloadMediaRejectsTruncatedPayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := secureTempDir(t)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: []byte("short")})
	builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
	file, err := DownloadMedia(context.Background(), builder, dir, 9, 20)
	require.ErrorContains(err, "size")
	assert.Nil(file)
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(entries)
}

func TestDownloadMediaRejectsSymlinkTemporaryDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges")
	}
	dir := secureTempDir(t)
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(secureTempDir(t), link))
	mock := tgmock.NewRequire(t)
	builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
	file, err := DownloadMedia(context.Background(), builder, link, -1, 10)
	require.ErrorContains(t, err, "directory")
	assert.Nil(t, file)
}

func TestDownloadMediaHonorsCancellationBeforeRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock := tgmock.NewRequire(t)
	builder := downloader.NewDownloader().Download(tg.NewClient(mock), &tg.InputDocumentFileLocation{ID: 123})
	file, err := DownloadMedia(ctx, builder, secureTempDir(t), -1, 10)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, file)
}
