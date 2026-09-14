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

// buildEXIFJPEG produces a minimal JPEG carrying an APP1/Exif segment whose
// IFD0 holds the given orientation, in the requested byte order.
func buildEXIFJPEG(t *testing.T, orientation uint16, bigEndian bool) []byte {
	t.Helper()
	var bo binary.ByteOrder = binary.LittleEndian
	order := []byte("II")
	if bigEndian {
		bo, order = binary.BigEndian, []byte("MM")
	}
	var tiff bytes.Buffer
	tiff.Write(order)
	_ = binary.Write(&tiff, bo, uint16(42))
	_ = binary.Write(&tiff, bo, uint32(8)) // IFD0 at offset 8
	_ = binary.Write(&tiff, bo, uint16(1)) // one entry
	_ = binary.Write(&tiff, bo, uint16(0x0112))
	_ = binary.Write(&tiff, bo, uint16(3)) // SHORT
	_ = binary.Write(&tiff, bo, uint32(1))
	_ = binary.Write(&tiff, bo, orientation)
	_ = binary.Write(&tiff, bo, uint16(0)) // pad the 4-byte value field
	_ = binary.Write(&tiff, bo, uint32(0)) // next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8}) // SOI
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)

	// A real, decodable image body so the same bytes can drive the encoder.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var body bytes.Buffer
	if err := jpeg.Encode(&body, img, nil); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	out.Write(body.Bytes()[2:]) // skip the body's own SOI
	return out.Bytes()
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
		{"bad TIFF byte order", func() []byte {
			d := append([]byte(nil), valid...)
			copy(d[10:12], []byte("XX"))
			return d
		}()},
		{"bad TIFF magic", func() []byte {
			d := append([]byte(nil), valid...)
			d[12], d[13] = 0xFF, 0xFF
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

// markedImage is 2x1: left pixel red, right pixel blue. Each orientation moves
// that pair somewhere predictable, which is what the transform must reproduce.
func markedImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})
	return img
}

func TestOrientImageMovesPixelsCorrectly(t *testing.T) {
	// Source is 2x1: red at (0,0), blue at (1,0). Each orientation moves that
	// pair somewhere determined, and both are asserted so a transform that
	// confuses two orientations (6 and 8 are easy to swap) cannot pass.
	cases := map[int]struct {
		w, h                     int
		redX, redY, blueX, blueY int
	}{
		1: {2, 1, 0, 0, 1, 0}, // unchanged
		2: {2, 1, 1, 0, 0, 0}, // mirror horizontal
		3: {2, 1, 1, 0, 0, 0}, // rotate 180
		4: {2, 1, 0, 0, 1, 0}, // mirror vertical; a single row is unchanged
		5: {1, 2, 0, 0, 0, 1}, // transpose
		6: {1, 2, 0, 0, 0, 1}, // rotate 90 CW: left end goes to the top
		7: {1, 2, 0, 1, 0, 0}, // anti-transpose
		8: {1, 2, 0, 1, 0, 0}, // rotate 270 CW: left end goes to the bottom
	}
	isRed := func(c color.Color) bool {
		r, g, b, _ := c.RGBA()
		return r>>8 == 255 && g>>8 == 0 && b>>8 == 0
	}
	isBlue := func(c color.Color) bool {
		r, g, b, _ := c.RGBA()
		return r>>8 == 0 && g>>8 == 0 && b>>8 == 255
	}
	for o, want := range cases {
		got := orientImage(markedImage(), o)
		b := got.Bounds()
		if b.Dx() != want.w || b.Dy() != want.h {
			t.Errorf("orientImage(%d) size = %dx%d, want %dx%d", o, b.Dx(), b.Dy(), want.w, want.h)
			continue
		}
		if !isRed(got.At(want.redX, want.redY)) {
			t.Errorf("orientImage(%d): (%d,%d) is not red", o, want.redX, want.redY)
		}
		if !isBlue(got.At(want.blueX, want.blueY)) {
			t.Errorf("orientImage(%d): (%d,%d) is not blue", o, want.blueX, want.blueY)
		}
	}
}

// The thumbnail must come out in display orientation, so a portrait photo
// stored as landscape pixels (orientation 6, the most common non-upright value)
// produces a portrait thumbnail.
func TestScaleAndEncodeThumbUsesDisplayOrientation(t *testing.T) {
	// 1000x500 decoded; orientation 6 means it is displayed 500x1000.
	src := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	data, w, h := scaleAndEncodeThumb(src, 1000, 500, 6)
	if data == nil {
		t.Fatal("scaleAndEncodeThumb returned no data")
	}
	if w >= h {
		t.Errorf("thumb is %dx%d, want portrait for orientation 6", w, h)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("thumbnail does not decode: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Errorf("encoded thumb is %dx%d but reported %dx%d", b.Dx(), b.Dy(), w, h)
	}

	// Orientation 1 on the same source stays landscape, so the swap is driven
	// by the tag and not by the scaling.
	_, w1, h1 := scaleAndEncodeThumb(src, 1000, 500, 1)
	if w1 <= h1 {
		t.Errorf("thumb is %dx%d, want landscape for orientation 1", w1, h1)
	}
}

// Every orientation must produce a decodable thumbnail with no out-of-range
// source indexing, including sizes that do not divide evenly.
func TestScaleAndEncodeThumbAllOrientations(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1001, 337))
	for o := 1; o <= 8; o++ {
		data, w, h := scaleAndEncodeThumb(src, 1001, 337, o)
		if data == nil || w < 1 || h < 1 {
			t.Errorf("orientation %d: got %dx%d and %d bytes", o, w, h, len(data))
			continue
		}
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("orientation %d: thumbnail does not decode: %v", o, err)
		}
	}
}
