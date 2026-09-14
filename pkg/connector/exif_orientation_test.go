// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for EXIF orientation handling in generated thumbnails.

package connector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// buildTIFF produces a raw TIFF/EXIF block whose IFD0 holds one Orientation
// entry. One builder for every case: three near-copies drifted apart before,
// and that is how the APP1 offset bug in the failure tests survived.
func buildTIFF(orientation uint32, bigEndian bool, typ uint16) []byte {
	return buildTIFFEntries(orientation, bigEndian, typ, nil, nil)
}

// exifDecoy is a non-Orientation IFD0 entry whose value fits the inline 4-byte
// field, used to pad IFD0 the way a real camera file does.
type exifDecoy struct {
	tag   uint16
	typ   uint16
	count uint32
}

// Tags a phone actually writes ahead of Orientation (0x0112), in the ascending
// order the TIFF spec requires: ImageWidth, ImageLength, Make, Model. Make and
// Model are cut to three characters so their value stays inline rather than
// becoming an offset, which keeps the fixture self-contained.
var exifDecoysBefore = []exifDecoy{
	{tag: 0x0100, typ: 4, count: 1}, // ImageWidth  (LONG)
	{tag: 0x0101, typ: 4, count: 1}, // ImageLength (LONG)
	{tag: 0x010F, typ: 2, count: 3}, // Make        (ASCII)
	{tag: 0x0110, typ: 2, count: 3}, // Model       (ASCII)
}

// And two that follow it, so a walk that runs on past Orientation is not
// mistaken for one that stopped at it.
var exifDecoysAfter = []exifDecoy{
	{tag: 0x011A, typ: 5, count: 1}, // XResolution     (RATIONAL)
	{tag: 0x0128, typ: 3, count: 1}, // ResolutionUnit  (SHORT)
}

// buildTIFFEntries produces a raw TIFF/EXIF block whose IFD0 holds the given
// decoy entries around one Orientation entry.
func buildTIFFEntries(orientation uint32, bigEndian bool, typ uint16, before, after []exifDecoy) []byte {
	var bo binary.ByteOrder = binary.LittleEndian
	order := []byte("II")
	if bigEndian {
		bo, order = binary.BigEndian, []byte("MM")
	}
	var b bytes.Buffer
	b.Write(order)
	_ = binary.Write(&b, bo, uint16(42))
	_ = binary.Write(&b, bo, uint32(8)) // IFD0 at offset 8
	_ = binary.Write(&b, bo, uint16(len(before)+1+len(after)))
	writeDecoy := func(d exifDecoy) {
		_ = binary.Write(&b, bo, d.tag)
		_ = binary.Write(&b, bo, d.typ)
		_ = binary.Write(&b, bo, d.count)
		// A plausible non-zero value. If the walk mistook one of these for the
		// Orientation entry it would read something out of the 1..8 range and
		// the assertion would show which.
		_ = binary.Write(&b, bo, uint32(0x00414141))
	}
	for _, d := range before {
		writeDecoy(d)
	}
	_ = binary.Write(&b, bo, uint16(0x0112))
	_ = binary.Write(&b, bo, typ)
	_ = binary.Write(&b, bo, uint32(1))
	if typ == 4 { // LONG fills the whole value field
		_ = binary.Write(&b, bo, orientation)
	} else { // SHORT sits in the first half
		_ = binary.Write(&b, bo, uint16(orientation))
		_ = binary.Write(&b, bo, uint16(0))
	}
	for _, d := range after {
		writeDecoy(d)
	}
	_ = binary.Write(&b, bo, uint32(0)) // no next IFD
	return b.Bytes()
}

// wrapJPEG wraps TIFF blocks as APP1/Exif segments, in order, ahead of a real
// image body. Extra segments model the APP0 JFIF or XMP that most re-saved
// JPEGs carry before the Exif one.
func wrapJPEG(t *testing.T, pre [][]byte, tiff []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8}) // SOI
	for _, seg := range pre {
		out.Write(seg)
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var body bytes.Buffer
	if err := jpeg.Encode(&body, img, nil); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	out.Write(body.Bytes()[2:]) // skip the body's own SOI
	return out.Bytes()
}

func buildEXIFJPEG(t *testing.T, orientation uint16, bigEndian bool) []byte {
	t.Helper()
	return wrapJPEG(t, nil, buildTIFF(uint32(orientation), bigEndian, 3))
}

func TestExifOrientationReadsAllValues(t *testing.T) {
	for want := 1; want <= 8; want++ {
		for _, be := range []bool{false, true} {
			data := buildEXIFJPEG(t, uint16(want), be)
			if got := exifOrientation(data); got != want {
				label := "little-endian"
				if be {
					label = "big-endian"
				}
				t.Errorf("exifOrientation(%s, orientation %d) = %d", label, want, got)
			}
		}
	}
}

// Anything unreadable must degrade to 1 rather than error: an unrotated
// thumbnail is the pre-fix behaviour, so failing to parse can only leave things
// as they were, never make them worse.
func TestExifOrientationDegradesToNormal(t *testing.T) {
	valid := buildEXIFJPEG(t, 6, false)
	cases := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"not a jpeg", []byte("PNG\x0d\x0a\x1a\x0a not a jpeg at all")},
		{"jpeg with no APP1", func() []byte {
			var b bytes.Buffer
			img := image.NewRGBA(image.Rect(0, 0, 2, 2))
			_ = jpeg.Encode(&b, img, nil)
			return b.Bytes()
		}()},
		{"truncated mid-segment", valid[:12]},
		{"segment length overruns the buffer", func() []byte {
			d := append([]byte(nil), valid...)
			binary.BigEndian.PutUint16(d[4:6], 0xFFFF)
			return d
		}()},
		// APP1 layout: FFD8 | FFE1 | len(2) | "Exif\0\0"(6) | byte order(2) |
		// magic(2) | IFD0 offset(4). So byte order is at 12-13 and magic at
		// 14-15; an earlier version of these two cases overwrote 10-11 and
		// 12-13, which corrupted the Exif header and the byte order instead —
		// deleting the magic check still passed.
		{"bad TIFF byte order", func() []byte {
			d := append([]byte(nil), valid...)
			copy(d[12:14], []byte("XX"))
			return d
		}()},
		{"bad TIFF magic", func() []byte {
			d := append([]byte(nil), valid...)
			d[14], d[15] = 0xFF, 0xFF
			return d
		}()},
		{"corrupt Exif header", func() []byte {
			d := append([]byte(nil), valid...)
			copy(d[6:12], []byte("XXXXXX"))
			return d
		}()},
		{"orientation out of range", buildEXIFJPEG(t, 99, false)},
		{"orientation zero", buildEXIFJPEG(t, 0, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exifOrientation(tc.data); got != orientationNormal {
				t.Errorf("exifOrientation() = %d, want 1", got)
			}
		})
	}
}

func TestDisplayDimsSwapsOnlyQuarterTurns(t *testing.T) {
	for o := 1; o <= 8; o++ {
		w, h := displayDims(100, 200, o)
		swapped := w == 200 && h == 100
		want := o >= 5 && o <= 8
		if swapped != want {
			t.Errorf("displayDims(100,200,%d) = (%d,%d); swapped=%v want %v", o, w, h, swapped, want)
		}
	}
}

// orientedCorners maps an EXIF orientation to markerImage's four corners as
// they must appear once displayed, read clockwise from the top-left, for a
// source whose corners are R/G/W/B in that order.
//
// Derived from the EXIF definitions rather than from the implementation: 2
// flips horizontally, 3 rotates 180, 4 flips vertically, 5 transposes about the
// main diagonal, 6 rotates 90 clockwise, 7 transposes about the anti-diagonal,
// 8 rotates 270 clockwise. Shared by the thumbnail and full-size tests so the
// two cannot drift apart.
var orientedCorners = map[int]string{
	1: "RGWB", // identity
	2: "GRBW", // flip horizontal
	3: "WBRG", // rotate 180
	4: "BWGR", // flip vertical
	5: "RBWG", // transpose
	6: "BRGW", // rotate 90 CW
	7: "WGRB", // anti-transpose
	8: "GWBR", // rotate 270 CW
}

// markerImage is a 1200x800 image of four solid quadrants:
//
//	R | G      R=red   G=green
//	--+--      B=blue  W=white
//	B | W
//
// Large and solid on purpose. A tiny source is UPSCALED to the 800px cap, and
// nearest-neighbour plus JPEG then smears single-pixel markers into nothing;
// quadrants survive both. Asymmetric in each axis with four distinct corners,
// so all eight orientations are distinguishable — a 2x1 source cannot separate
// a mirror from a rotation, since orientations 1/4, 2/3, 5/6 and 7/8 produce
// identical output on it.
func markerImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	quad := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, c)
			}
		}
	}
	quad(0, 0, 600, 400, color.RGBA{R: 255, A: 255})
	quad(600, 0, 1200, 400, color.RGBA{G: 255, A: 255})
	quad(0, 400, 600, 800, color.RGBA{B: 255, A: 255})
	quad(600, 400, 1200, 800, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	return img
}

// nearestMarker names the marker colour a (lossy JPEG) pixel is closest to.
// Channels are converted to signed before subtracting: doing the arithmetic on
// the uint32 from RGBA() underflows, and squaring that overflows to a negative
// distance, which silently makes one marker always win.
func nearestMarker(c color.Color) string {
	r32, g32, b32, _ := c.RGBA()
	r, g, b := int(r32>>8), int(g32>>8), int(b32>>8)
	markers := []struct {
		name    string
		r, g, b int
	}{
		{"R", 255, 0, 0}, {"G", 0, 255, 0}, {"B", 0, 0, 255}, {"W", 255, 255, 255},
	}
	best, bestD := ".", 1<<30
	for _, m := range markers {
		d := (r-m.r)*(r-m.r) + (g-m.g)*(g-m.g) + (b-m.b)*(b-m.b)
		if d < bestD {
			best, bestD = m.name, d
		}
	}
	return best
}

// TestScaleAndEncodeThumbOrientsPixels drives the PRODUCTION thumbnail path
// rather than a helper only tests call, and checks where pixels land rather
// than only the output size.
//
// Expected values are the EXIF definitions applied to the source's corners,
// derived independently of the implementation: 2 flips horizontally, 3 rotates
// 180, 4 flips vertically, 5 transposes about the main diagonal, 6 rotates 90
// clockwise, 7 transposes about the anti-diagonal, 8 rotates 270 clockwise.
func TestScaleAndEncodeThumbOrientsPixels(t *testing.T) {
	want := orientedCorners
	for o := 1; o <= 8; o++ {
		data, w, h := scaleAndEncodeThumb(markerImage(), o)
		if data == nil {
			t.Errorf("orientation %d: no thumbnail", o)
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Errorf("orientation %d: does not decode: %v", o, err)
			continue
		}
		if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
			t.Errorf("orientation %d: encoded %dx%d but reported %dx%d",
				o, img.Bounds().Dx(), img.Bounds().Dy(), w, h)
		}
		// Sample inside each quadrant, away from the seams and the JPEG
		// ringing at block edges.
		at := func(fx, fy float64) string {
			return nearestMarker(img.At(int(float64(w)*fx), int(float64(h)*fy)))
		}
		got := at(0.25, 0.25) + at(0.75, 0.25) + at(0.75, 0.75) + at(0.25, 0.75)
		if got != want[o] {
			t.Errorf("orientation %d: corners clockwise from top-left = %q, want %q (thumb %dx%d)",
				o, got, want[o], w, h)
		}
	}
}

// The thumbnail must be sized in display orientation, so a landscape-decoded
// portrait photo produces a portrait thumbnail.
func TestScaleAndEncodeThumbUsesDisplayOrientation(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	if _, w, h := scaleAndEncodeThumb(src, 6); w >= h {
		t.Errorf("orientation 6 thumb is %dx%d, want portrait", w, h)
	}
	if _, w, h := scaleAndEncodeThumb(src, 1); w <= h {
		t.Errorf("orientation 1 thumb is %dx%d, want landscape", w, h)
	}
}

// Sizes that do not divide evenly must still produce a decodable thumbnail for
// every orientation.
func TestScaleAndEncodeThumbOddSizes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1001, 337))
	for o := 1; o <= 8; o++ {
		data, w, h := scaleAndEncodeThumb(src, o)
		if data == nil || w < 1 || h < 1 {
			t.Errorf("orientation %d: got %dx%d, %d bytes", o, w, h, len(data))
			continue
		}
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("orientation %d: does not decode: %v", o, err)
		}
	}
}

// Most re-saved JPEGs begin with an APP0 JFIF segment, so APP1 is not the first
// thing after SOI and the segment-skip has to work. Nothing covered that.
func TestExifOrientationSkipsEarlierSegments(t *testing.T) {
	valid := buildEXIFJPEG(t, 6, false)
	app0 := []byte{0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0,
		1, 1, 0, 0, 1, 0, 1, 0, 0}
	withAPP0 := append(append(append([]byte{}, valid[:2]...), app0...), valid[2:]...)
	if got := exifOrientation(withAPP0); got != 6 {
		t.Errorf("exifOrientation() = %d with an APP0 before APP1, want 6", got)
	}

	// And with 0xFF fill bytes before the APP1 marker, which the spec allows.
	withFill := append(append(append([]byte{}, valid[:2]...), 0xFF, 0xFF), valid[2:]...)
	if got := exifOrientation(withFill); got != 6 {
		t.Errorf("exifOrientation() = %d with 0xFF fill before the marker, want 6", got)
	}
}

// The IFD entry's type field must be honoured. A big-endian LONG read as a
// SHORT yields the high half, which is zero for every real orientation — so
// ignoring the type silently degrades to 1 rather than failing loudly.
func TestExifOrientationHonoursEntryType(t *testing.T) {
	for _, be := range []bool{false, true} {
		if got := exifOrientation(wrapJPEG(t, nil, buildTIFF(6, be, 4))); got != 6 {
			t.Errorf("exifOrientation(bigEndian=%v, LONG) = %d, want 6", be, got)
		}
	}
	// An unknown type must not be guessed at.
	if got := exifOrientation(wrapJPEG(t, nil, buildTIFF(6, false, 9))); got != orientationNormal {
		t.Errorf("exifOrientation(unknown type) = %d, want 1", got)
	}
}

// A TIFF file's header is the same block the EXIF path parses, so its
// orientation is readable directly. Callers re-encode TIFF to JPEG without EXIF
// but rotate the pixels first, so the uploaded image, its dimensions and its
// thumbnail all agree.
func TestExifOrientationReadsBareTIFF(t *testing.T) {
	for _, be := range []bool{false, true} {
		if got := exifOrientation(buildTIFF(8, be, 3)); got != 8 {
			t.Errorf("exifOrientation(bare TIFF, bigEndian=%v) = %d, want 8", be, got)
		}
	}
	// A JPEG must still take the APP1 path rather than being read as TIFF.
	if got := exifOrientation(buildEXIFJPEG(t, 3, false)); got != 3 {
		t.Errorf("exifOrientation(JPEG) = %d, want 3", got)
	}
}

// A non-Exif APP1 (XMP is the common one) must be skipped rather than ending
// the search, and the walk must stop at SOS instead of reading scan data as
// segment headers.
func TestExifOrientationSkipsNonExifAPP1(t *testing.T) {
	// Segment length counts itself: 8 payload bytes + 2 = 0x000A.
	xmp := []byte{0xFF, 0xE1, 0x00, 0x0A, 'h', 't', 't', 'p', ':', 0, 0, 0}
	if got := exifOrientation(wrapJPEG(t, [][]byte{xmp}, buildTIFF(6, false, 3))); got != 6 {
		t.Errorf("exifOrientation() = %d with an XMP APP1 before the Exif one, want 6", got)
	}
	// Exif after SOS is not metadata; the walk must not go looking.
	sos := []byte{0xFF, 0xDA, 0x00, 0x08, 1, 1, 0, 0, 0, 0}
	if got := exifOrientation(wrapJPEG(t, [][]byte{sos}, buildTIFF(6, false, 3))); got != orientationNormal {
		t.Errorf("exifOrientation() = %d for Exif after SOS, want 1", got)
	}
}

// An IFD0 offset pointing inside the 8-byte TIFF header is malformed, and the
// guard against it is load-bearing rather than decorative.
//
// With the offset at 0, the walk reads the "II" magic as an entry count and
// then finds an entry inside the block's own bytes. This particular layout —
// found by brute-forcing every offset below 8 against the parser with and
// without the guard — resolves to a bogus orientation of 3. A first attempt at
// this test used offset 4, which happens to resolve to 1 anyway, so removing
// the guard still passed.
func TestExifOrientationRejectsIFDInsideHeader(t *testing.T) {
	tiff := []byte{'I', 'I', 42, 0, 0, 0, 0, 0} // magic 42, IFD0 offset 0
	for range 4 {
		tiff = append(tiff, 0x12, 0x01, 0x03, 0x00, 0x00, 0x00)
	}
	if got := exifOrientation(tiff); got != orientationNormal {
		t.Errorf("exifOrientation(IFD inside the header) = %d, want 1", got)
	}
	if got := exifOrientation(wrapJPEG(t, nil, tiff)); got != orientationNormal {
		t.Errorf("exifOrientation(same, wrapped in APP1) = %d, want 1", got)
	}
}

// A real camera JPEG's IFD0 carries a dozen tags and Orientation is not the
// first. Every other fixture here builds a single-entry IFD0, which never
// exercises the walk past a non-matching tag — so a parser that gave up on the
// first entry instead of continuing would pass the whole suite while failing on
// every photo a phone produces.
func TestExifOrientationWalksPastOtherTags(t *testing.T) {
	for want := 1; want <= 8; want++ {
		for _, be := range []bool{false, true} {
			tiff := buildTIFFEntries(uint32(want), be, 3, exifDecoysBefore, exifDecoysAfter)
			for _, tc := range []struct {
				name string
				data []byte
			}{
				{"bare TIFF", tiff},
				{"JPEG APP1", wrapJPEG(t, nil, tiff)},
			} {
				if got := exifOrientation(tc.data); got != want {
					t.Errorf("%s (orientation %d, bigEndian=%v): got %d, want %d",
						tc.name, want, be, got, want)
				}
			}
		}
	}
}

// markerSubImage returns markerImage's content as a sub-image of a larger
// black-bordered canvas, so its Pix slice and Bounds.Min start at (100, 100)
// rather than the origin.
//
// orientImage reads the source two different ways depending on its type — by
// Bounds().Min through At in the generic path, and from Pix[0] in the
// four-byte-per-pixel fast path — and both have to land on the same pixel. An
// origin-anchored fixture cannot tell a correct offset from a missing one,
// because Min is (0, 0) either way. Here a mistake pulls in the black border.
func markerSubImage() image.Image {
	canvas := image.NewRGBA(image.Rect(0, 0, 1400, 1000))
	for y := range 1000 {
		for x := range 1400 {
			canvas.Set(x, y, color.RGBA{A: 255})
		}
	}
	marker := markerImage()
	for y := range 800 {
		for x := range 1200 {
			canvas.Set(100+x, 100+y, marker.At(x, y))
		}
	}
	return canvas.SubImage(image.Rect(100, 100, 1300, 900))
}

// genericImage hides an image's concrete type so orientImage cannot take its
// Pix fast path and must fall back to At/Set.
type genericImage struct{ image.Image }

// cornersOf reads img's four corner quadrants clockwise from the top-left.
func cornersOf(img image.Image) string {
	b := img.Bounds()
	at := func(fx, fy float64) string {
		return nearestMarker(img.At(
			b.Min.X+int(float64(b.Dx())*fx),
			b.Min.Y+int(float64(b.Dy())*fy)))
	}
	return at(0.25, 0.25) + at(0.75, 0.25) + at(0.75, 0.75) + at(0.25, 0.75)
}

// orientImage rotates the FULL-SIZE image — the path a TIFF takes, since it is
// re-encoded as a JPEG that carries no EXIF — and had no test at all.
func TestOrientImageOrientsPixels(t *testing.T) {
	for o := 1; o <= 8; o++ {
		for _, src := range []struct {
			name string
			img  image.Image
		}{
			{"RGBA", markerImage()},
			{"NRGBA", asNRGBA(markerImage())},
			{"generic", genericImage{markerImage()}},
			{"sub-image", markerSubImage()},
			{"generic sub-image", genericImage{markerSubImage()}},
		} {
			got := orientImage(src.img, o)
			wantW, wantH := displayDims(1200, 800, o)
			if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
				t.Errorf("orientation %d (%s): size %dx%d, want %dx%d", o, src.name,
					got.Bounds().Dx(), got.Bounds().Dy(), wantW, wantH)
			}
			if c := cornersOf(got); c != orientedCorners[o] {
				t.Errorf("orientation %d (%s): corners clockwise from top-left = %q, want %q",
					o, src.name, c, orientedCorners[o])
			}
		}
	}
}

// asNRGBA re-renders an image in the other four-byte layout orientImage has a
// fast path for. The markers are opaque, so the two encodings hold identical
// bytes and any difference in output comes from the code, not the format.
func asNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := range b.Dy() {
		for x := range b.Dx() {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// The byte-copy fast path is an optimisation, so it has to be indistinguishable
// from the generic path it replaces — pixel for pixel, not just corner for
// corner, which a transposition bug in the interior could survive.
func TestOrientImageFastPathMatchesGenericPath(t *testing.T) {
	for o := 1; o <= 8; o++ {
		for _, src := range []struct {
			name string
			img  image.Image
		}{
			{"RGBA", markerImage()},
			{"NRGBA", asNRGBA(markerImage())},
			{"sub-image", markerSubImage()},
		} {
			fast := orientImage(src.img, o)
			slow := orientImage(genericImage{src.img}, o)
			if diff := firstPixelDiff(fast, slow); diff != "" {
				t.Errorf("orientation %d (%s): fast path differs from generic path at %s",
					o, src.name, diff)
			}
		}
	}
}

// firstPixelDiff returns a description of the first differing pixel, or "" if
// the two images match.
func firstPixelDiff(a, b image.Image) string {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return fmt.Sprintf("size %dx%d vs %dx%d", ab.Dx(), ab.Dy(), bb.Dx(), bb.Dy())
	}
	for y := range ab.Dy() {
		for x := range ab.Dx() {
			ar, ag, al, aa := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			br, bg, bl, ba := b.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			if ar != br || ag != bg || al != bl || aa != ba {
				return fmt.Sprintf("(%d,%d): %v,%v,%v,%v vs %v,%v,%v,%v",
					x, y, ar, ag, al, aa, br, bg, bl, ba)
			}
		}
	}
	return ""
}

// An upright image must come back untouched — the same image, not a copy —
// since orientImage runs on every full-size TIFF and copying a 24-megapixel
// frame to change nothing is the common case.
func TestOrientImageLeavesUprightImagesAlone(t *testing.T) {
	src := markerImage()
	for _, o := range []int{orientationNormal, 0, -1, 9, 100} {
		if got := orientImage(src, o); got != src {
			t.Errorf("orientation %d: returned a new image, want the input unchanged", o)
		}
	}
}

// scaleAndEncodeThumb indexes the source through its Bounds().Min, which every
// fixture here leaves at the origin because that is what the decoders return. A
// sub-image is the one shape that tells a correct offset from an absent one.
func TestScaleAndEncodeThumbHonoursSourceBoundsMin(t *testing.T) {
	for o := 1; o <= 8; o++ {
		want, wantW, wantH := scaleAndEncodeThumb(markerImage(), o)
		got, gotW, gotH := scaleAndEncodeThumb(markerSubImage(), o)
		if gotW != wantW || gotH != wantH {
			t.Errorf("orientation %d: sub-image thumb is %dx%d, want %dx%d", o, gotW, gotH, wantW, wantH)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("orientation %d: sub-image thumb differs from the origin-anchored one; "+
				"Bounds().Min is being ignored", o)
		}
	}
}

// Rotating first and then scaling upright must give the same thumbnail as
// scaling with the orientation applied inline. If it does not, one of the two
// mappings is wrong, or a TIFF — which goes through orientImage for its
// full-size image and the inline path for its thumbnail — ends up rotated
// twice.
func TestScaleAndEncodeThumbAgreesWithOrientImage(t *testing.T) {
	for o := 1; o <= 8; o++ {
		inline, iw, ih := scaleAndEncodeThumb(markerImage(), o)
		pre, pw, ph := scaleAndEncodeThumb(orientImage(markerImage(), o), orientationNormal)
		if iw != pw || ih != ph {
			t.Errorf("orientation %d: inline thumb %dx%d, pre-rotated %dx%d", o, iw, ih, pw, ph)
			continue
		}
		if !bytes.Equal(inline, pre) {
			t.Errorf("orientation %d: scaleAndEncodeThumb(img, o) differs from "+
				"scaleAndEncodeThumb(orientImage(img, o), 1)", o)
		}
	}
}

// A truncated IFD must not be walked off the end. The entry count is a
// 16-bit field read from the file, so a corrupt or clipped block can claim
// far more entries than the bytes hold; the bound is what stops the read.
func TestExifOrientationRejectsTruncatedIFD(t *testing.T) {
	tiff := buildTIFFEntries(6, false, 3, exifDecoysBefore, nil)
	// Keep the header and the entry count, drop the entries. The count still
	// says five, so an unguarded walk reads past the end.
	truncated := tiff[:12]
	if got := exifOrientation(truncated); got != orientationNormal {
		t.Errorf("truncated IFD returned %d, want %d", got, orientationNormal)
	}
	// And the same block one entry short of what it promises.
	short := tiff[:len(tiff)-13]
	if got := exifOrientation(short); got != orientationNormal {
		t.Errorf("IFD one entry short returned %d, want %d", got, orientationNormal)
	}
}
