// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for EXIF orientation handling in generated thumbnails.

package connector

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// buildTIFF produces a raw TIFF/EXIF block whose IFD0 holds one Orientation
// entry. One builder for every case: three near-copies drifted apart before,
// and that is how the APP1 offset bug in the failure tests survived.
func buildTIFF(orientation uint32, bigEndian bool, typ uint16) []byte {
	var bo binary.ByteOrder = binary.LittleEndian
	order := []byte("II")
	if bigEndian {
		bo, order = binary.BigEndian, []byte("MM")
	}
	var b bytes.Buffer
	b.Write(order)
	_ = binary.Write(&b, bo, uint16(42))
	_ = binary.Write(&b, bo, uint32(8)) // IFD0 at offset 8
	_ = binary.Write(&b, bo, uint16(1)) // one entry
	_ = binary.Write(&b, bo, uint16(0x0112))
	_ = binary.Write(&b, bo, typ)
	_ = binary.Write(&b, bo, uint32(1))
	if typ == 4 { // LONG fills the whole value field
		_ = binary.Write(&b, bo, orientation)
	} else { // SHORT sits in the first half
		_ = binary.Write(&b, bo, uint16(orientation))
		_ = binary.Write(&b, bo, uint16(0))
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
	// Display corners read clockwise from top-left, for source R/G/W/B.
	want := map[int]string{
		1: "RGWB", // identity
		2: "GRBW", // flip horizontal
		3: "WBRG", // rotate 180
		4: "BWGR", // flip vertical
		5: "RBWG", // transpose
		6: "BRGW", // rotate 90 CW
		7: "WGRB", // anti-transpose
		8: "GWBR", // rotate 270 CW
	}
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
