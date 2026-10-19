package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMediaStore(t *testing.T) (*MediaStore, string) {
	t.Helper()

	dir := t.TempDir()
	store, err := NewMediaStore(dir)
	require.NoError(t, err)

	return store, dir
}

func TestMediaStorePutOriginalAndOpen(t *testing.T) {
	t.Parallel()

	store, dir := newTestMediaStore(t)
	data := []byte("original-png-bytes")

	ref, err := store.PutOriginal(context.Background(), "image/png", data)
	require.NoError(t, err)
	require.True(t, ref.Valid())
	assert.Equal(t, "images/"+ref.Base()+".png", ref.Path)

	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), ref.Digest)
	assert.Equal(t, int64(len(data)), ref.Bytes)

	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(ref.Path)))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := store.Open(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Returned bytes are detached.
	got[0] = 'X'
	again, err := store.Open(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, data, again)
}

func TestMediaStorePutOriginalIsContentAddressed(t *testing.T) {
	t.Parallel()

	store, _ := newTestMediaStore(t)
	first, err := store.PutOriginal(context.Background(), "image/jpeg", []byte("same"))
	require.NoError(t, err)

	second, err := store.PutOriginal(context.Background(), "image/jpeg", []byte("same"))
	require.NoError(t, err)
	assert.Equal(t, first.Path, second.Path)
}

func TestMediaStorePutModelCopySharesOriginalBase(t *testing.T) {
	t.Parallel()

	store, _ := newTestMediaStore(t)
	original, err := store.PutOriginal(context.Background(), "image/png", []byte("original"))
	require.NoError(t, err)

	modelCopy, err := store.PutModelCopy(context.Background(), original, "image/jpeg", []byte("normalized"))
	require.NoError(t, err)
	assert.Equal(t, "assets/"+original.Base()+".jpg", modelCopy.Path)
	assert.NotEqual(t, original.Digest, modelCopy.Digest)

	got, err := store.Open(context.Background(), modelCopy)
	require.NoError(t, err)
	assert.Equal(t, []byte("normalized"), got)
}

func TestMediaStoreOpenRejectsCorruptionAndMissing(t *testing.T) {
	t.Parallel()

	store, dir := newTestMediaStore(t)
	ref, err := store.PutOriginal(context.Background(), "image/png", []byte("bytes"))
	require.NoError(t, err)

	tampered := ref
	tampered.Digest = "00" + ref.Digest[2:]
	_, err = store.Open(context.Background(), tampered)
	require.ErrorIs(t, err, ErrMediaCorrupt)

	missing := ref
	missing.Path = "images/image-0000000000000000.png"
	_, err = store.Open(context.Background(), missing)
	require.ErrorIs(t, err, ErrMediaMissing)

	require.NoError(t, os.Remove(filepath.Join(dir, filepath.FromSlash(ref.Path))))
	_, err = store.Open(context.Background(), ref)
	require.ErrorIs(t, err, ErrMediaMissing)
}

func TestMediaStoreRejectsTraversalAndSymlink(t *testing.T) {
	t.Parallel()

	store, dir := newTestMediaStore(t)

	for _, path := range []string{
		"../escape.png",
		"images/../escape.png",
		"images/nested/image-0000000000000000.png",
		"images/image-0000000000000000.png/../../x",
		"other/image-0000000000000000.png",
		"images/not-a-media-name.png",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			_, err := store.Open(context.Background(), MediaRef{
				Path: path, Digest: hex.EncodeToString(make([]byte, sha256.Size)),
			})
			require.ErrorIs(t, err, ErrMediaInvalid)
		})
	}

	// A symlinked media directory must never be written through.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "outside"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, ImagesDirName)))

	_, err := store.PutOriginal(context.Background(), "image/png", []byte("x"))
	require.Error(t, err)

	// A symlinked media file must never be read.
	require.NoError(t, os.Remove(filepath.Join(dir, ImagesDirName)))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ImagesDirName), 0o700))
	target := filepath.Join(dir, "outside", "target.png")
	require.NoError(t, os.WriteFile(target, []byte("y"), 0o600))

	link := filepath.Join(dir, ImagesDirName, "image-0000000000000000.png")
	require.NoError(t, os.Symlink(target, link))

	_, err = store.Open(context.Background(), MediaRef{
		Path: "images/image-0000000000000000.png", Digest: hex.EncodeToString(make([]byte, sha256.Size)),
	})
	require.ErrorIs(t, err, ErrMediaInvalid)
}

func TestMediaStoreEnumerate(t *testing.T) {
	t.Parallel()

	store, _ := newTestMediaStore(t)

	original, err := store.PutOriginal(context.Background(), "image/png", []byte("original"))
	require.NoError(t, err)

	modelCopy, err := store.PutModelCopy(context.Background(), original, "image/jpeg", []byte("copy"))
	require.NoError(t, err)

	// Files that do not follow the media naming scheme are ignored.
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), ImagesDirName, "notes.txt"), []byte("x"), 0o600))

	refs, err := store.Enumerate(context.Background())
	require.NoError(t, err)

	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		assert.True(t, ref.Valid(), "an enumerated reference must be complete")
		paths = append(paths, ref.Path)
	}

	assert.ElementsMatch(t, []string{original.Path, modelCopy.Path}, paths)

	byPath := make(map[string]MediaRef, len(refs))
	for _, ref := range refs {
		byPath[ref.Path] = ref
	}

	assert.Equal(t, original.Digest, byPath[original.Path].Digest)
	assert.Equal(t, modelCopy.Digest, byPath[modelCopy.Path].Digest)
}

func TestMediaStoreGCKeepsReferencesAndOriginals(t *testing.T) {
	t.Parallel()

	store, _ := newTestMediaStore(t)

	original, err := store.PutOriginal(context.Background(), "image/png", []byte("kept-original"))
	require.NoError(t, err)

	modelCopy, err := store.PutModelCopy(context.Background(), original, "image/jpeg", []byte("kept-copy"))
	require.NoError(t, err)

	orphan, err := store.PutOriginal(context.Background(), "image/png", []byte("orphan"))
	require.NoError(t, err)

	report, err := store.GC(context.Background(), map[string]struct{}{modelCopy.Path: {}}, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []string{orphan.Path}, report.Deleted)

	// The referenced copy and its same-base original survive.
	_, err = store.Open(context.Background(), modelCopy)
	require.NoError(t, err)
	_, err = store.Open(context.Background(), original)
	require.NoError(t, err)
}

func TestMediaStoreGCGracePeriod(t *testing.T) {
	t.Parallel()

	store, dir := newTestMediaStore(t)

	orphan, err := store.PutOriginal(context.Background(), "image/png", []byte("orphan"))
	require.NoError(t, err)

	// A fresh file inside the grace window survives.
	report, err := store.GC(context.Background(), nil, time.Hour, false)
	require.NoError(t, err)
	assert.Empty(t, report.Deleted)

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, filepath.FromSlash(orphan.Path)), old, old))

	report, err = store.GC(context.Background(), nil, time.Hour, false)
	require.NoError(t, err)
	assert.Equal(t, []string{orphan.Path}, report.Deleted)
}

func TestMediaStoreRejectsUnsupportedMIMEType(t *testing.T) {
	t.Parallel()

	store, _ := newTestMediaStore(t)

	_, err := store.PutOriginal(context.Background(), "application/pdf", []byte("x"))
	require.ErrorIs(t, err, ErrMediaInvalid)
}

func TestMediaStoreRejectsWrongFileMode(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("mode policy is enforced on unix")
	}

	store, dir := newTestMediaStore(t)

	ref, err := store.PutOriginal(context.Background(), "image/png", []byte("bytes"))
	require.NoError(t, err)

	path := filepath.Join(dir, filepath.FromSlash(ref.Path))
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // the loosened mode is the case under test

	_, err = store.Open(context.Background(), ref)
	require.ErrorIs(t, err, ErrMediaInvalid)
}
