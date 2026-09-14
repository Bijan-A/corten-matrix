// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for thumbnail-generation versioning of the attachment cache.

package connector

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.mau.fi/util/dbutil"
)

func thumbCacheStore(t *testing.T) (*cloudBackfillStore, *dbutil.Database, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	return store, db, ctx
}

const cachedImageJSON = `{"msgtype":"m.image","body":"IMG.jpg",` +
	`"url":"mxc://example.org/full",` +
	`"info":{"w":800,"h":600,"mimetype":"image/jpeg",` +
	`"thumbnail_url":"mxc://example.org/thumb",` +
	`"thumbnail_info":{"w":800,"h":600}}}`

const cachedFileJSON = `{"msgtype":"m.file","body":"doc.pdf",` +
	`"url":"mxc://example.org/doc","info":{"mimetype":"application/pdf"}}`

// A stale entry must keep its full-size url and lose only the thumbnail.
// Withholding it entirely would discard a valid mxc URI and force a CloudKit
// re-download that can fail permanently, dropping the photo from the room.
func TestLoadAttachmentCacheStripsStaleThumbnailButKeepsImage(t *testing.T) {
	store, db, ctx := thumbCacheStore(t)

	// Stamped 0: written by a previous generation.
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts, thumb_version)
		VALUES ($1, 'stale-image', $2, 1000, 0)`, testSQLLoginID, []byte(cachedImageJSON)); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	// A stale entry with no thumbnail at all must be returned untouched.
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts, thumb_version)
		VALUES ($1, 'stale-file', $2, 1000, 0)`, testSQLLoginID, []byte(cachedFileJSON)); err != nil {
		t.Fatalf("insert stale file: %v", err)
	}
	// Current generation: untouched, thumbnail intact.
	store.saveAttachmentCacheEntry(ctx, "fresh-image", []byte(cachedImageJSON))

	cache, err := store.loadAttachmentCacheJSON(ctx)
	if err != nil {
		t.Fatalf("loadAttachmentCacheJSON: %v", err)
	}
	if len(cache) != 3 {
		t.Fatalf("loaded %d entries, want 3 — a stale entry must not be withheld", len(cache))
	}

	info := func(name string) map[string]any {
		t.Helper()
		var c map[string]any
		if err := json.Unmarshal(cache[name], &c); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if url, _ := c["url"].(string); url == "" {
			t.Errorf("%s: full-size url was dropped", name)
		}
		i, _ := c["info"].(map[string]any)
		return i
	}
	for _, key := range []string{"thumbnail_url", "thumbnail_info"} {
		if _, present := info("stale-image")[key]; present {
			t.Errorf("stale-image kept %s; it would be replayed sideways", key)
		}
	}
	if _, present := info("fresh-image")["thumbnail_url"]; !present {
		t.Error("fresh-image lost its thumbnail; only stale entries should be stripped")
	}
	if string(cache["stale-file"]) != cachedFileJSON {
		t.Error("a stale entry with no thumbnail was modified")
	}
}

// saveAttachmentCacheEntry must stamp the current version on both the insert
// and the update half of the upsert, or a rewritten entry stays stale forever.
func TestSaveAttachmentCacheEntryStampsVersionOnUpsert(t *testing.T) {
	store, db, ctx := thumbCacheStore(t)

	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts, thumb_version)
		VALUES ($1, 'rec', $2, 1000, 0)`, testSQLLoginID, []byte(cachedImageJSON)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Overwrite via the upsert's conflict path.
	store.saveAttachmentCacheEntry(ctx, "rec", []byte(cachedImageJSON))

	var version int
	if err := db.QueryRow(ctx,
		`SELECT thumb_version FROM cloud_attachment_cache WHERE login_id=$1 AND record_name='rec'`,
		testSQLLoginID).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != thumbCacheVersion {
		t.Errorf("thumb_version = %d after upsert, want %d", version, thumbCacheVersion)
	}
	// And it is now treated as current.
	cache, err := store.loadAttachmentCacheJSON(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var c map[string]any
	_ = json.Unmarshal(cache["rec"], &c)
	if info, _ := c["info"].(map[string]any); info["thumbnail_url"] == nil {
		t.Error("re-saved entry was stripped; the upsert did not stamp the version")
	}
}

// ensureSchema must be able to add thumb_version to a table that predates it,
// and must stay idempotent.
func TestThumbVersionColumnMigration(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	// A pre-migration table: no thumb_version.
	if _, err := db.Exec(ctx, `CREATE TABLE cloud_attachment_cache (
		login_id TEXT NOT NULL, record_name TEXT NOT NULL,
		content_json BLOB NOT NULL, created_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, record_name))`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO cloud_attachment_cache VALUES ($1, 'legacy', $2, 1000)`,
		testSQLLoginID, []byte(cachedImageJSON)); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	store := newCloudBackfillStore(db, testSQLLoginID)
	for pass := range 2 {
		if err := store.ensureSchema(ctx); err != nil {
			t.Fatalf("ensureSchema pass %d: %v", pass+1, err)
		}
	}
	// The legacy row survives, defaulted to 0, so it is stripped rather than lost.
	cache, err := store.loadAttachmentCacheJSON(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cache["legacy"]; !ok {
		t.Fatal("the legacy row disappeared across the migration")
	}
	var c map[string]any
	_ = json.Unmarshal(cache["legacy"], &c)
	info, _ := c["info"].(map[string]any)
	if _, present := info["thumbnail_url"]; present {
		t.Error("legacy row kept its thumbnail; it defaulted to the current version")
	}
	if url, _ := c["url"].(string); url == "" {
		t.Error("legacy row lost its full-size url")
	}
}

// stripCachedThumbnail must leave anything it cannot parse alone: a stale entry
// is better served as-is than dropped.
func TestStripCachedThumbnailLeavesUnparseableAlone(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"not json", "definitely not json"},
		{"no info object", `{"msgtype":"m.image","url":"mxc://x/y"}`},
		{"info is not an object", `{"info":"nope"}`},
		{"no thumbnail keys", cachedFileJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(stripCachedThumbnail([]byte(tc.in))); got != tc.in {
				t.Errorf("stripCachedThumbnail() = %q, want it unchanged", got)
			}
		})
	}
}

// The encrypted form of a cached image: in an E2EE portal the mxc URIs live in
// "file"/"thumbnail_file" objects and the plain "url"/"thumbnail_url" keys are
// absent entirely. Stripping only the plaintext keys would leave every
// encrypted room replaying its sideways thumbnails.
const cachedEncryptedImageJSON = `{"msgtype":"m.image","body":"IMG.jpg",` +
	`"filename":"IMG.jpg",` +
	`"file":{"url":"mxc://example.org/full-enc","v":"v2",` +
	`"key":{"alg":"A256CTR","ext":true,"k":"AAA","key_ops":["encrypt","decrypt"],"kty":"oct"},` +
	`"iv":"BBB","hashes":{"sha256":"CCC"}},` +
	`"info":{"w":4032,"h":3024,"size":2048576,"mimetype":"image/jpeg",` +
	`"xyz.amorgan.blurhash":"LEHV6nWB",` +
	`"thumbnail_file":{"url":"mxc://example.org/thumb-enc","v":"v2",` +
	`"key":{"alg":"A256CTR","ext":true,"k":"DDD","key_ops":["encrypt","decrypt"],"kty":"oct"},` +
	`"iv":"EEE","hashes":{"sha256":"FFF"}},` +
	`"thumbnail_info":{"w":800,"h":600,"size":40960,"mimetype":"image/jpeg"}}}`

// The strip has to be surgical: it removes three keys and must leave the
// document otherwise byte-for-byte equivalent. Asserting only that the
// thumbnail is gone would pass just as well if the encryption key, the blurhash
// or the filename went with it — and losing "file" would lose the photo.
func TestStripCachedThumbnailRemovesEncryptedThumbnailAndKeepsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"plaintext", cachedImageJSON},
		{"encrypted", cachedEncryptedImageJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Expected value built by deleting the three keys from the parsed
			// fixture, so the assertion describes the intent rather than
			// restating whatever the function happens to emit.
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.input), &want); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			wantInfo := want["info"].(map[string]any)
			removed := 0
			for _, key := range []string{"thumbnail_url", "thumbnail_file", "thumbnail_info"} {
				if _, present := wantInfo[key]; present {
					removed++
				}
				delete(wantInfo, key)
			}
			if removed == 0 {
				t.Fatal("fixture carries no thumbnail keys, so this test proves nothing")
			}

			var got map[string]any
			if err := json.Unmarshal(stripCachedThumbnail([]byte(tc.input)), &got); err != nil {
				t.Fatalf("unmarshal stripped: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("stripped document differs from fixture-minus-thumbnail\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// The version gate has to run on encrypted entries too, through the real
// database path rather than only the helper.
func TestLoadAttachmentCacheStripsEncryptedThumbnail(t *testing.T) {
	store, db, ctx := thumbCacheStore(t)
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts, thumb_version)
		VALUES ($1, 'stale-enc', $2, 1000, 0)`, testSQLLoginID, []byte(cachedEncryptedImageJSON)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	cache, err := store.loadAttachmentCacheJSON(ctx)
	if err != nil {
		t.Fatalf("loadAttachmentCacheJSON: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(cache["stale-enc"], &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := c["file"]; !present {
		t.Fatal("the encrypted full-size file was dropped; the photo is gone, not just its preview")
	}
	info := c["info"].(map[string]any)
	for _, key := range []string{"thumbnail_file", "thumbnail_info"} {
		if _, present := info[key]; present {
			t.Errorf("kept %s; it would be replayed sideways", key)
		}
	}
}
