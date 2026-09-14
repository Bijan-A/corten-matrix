// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// EXIF orientation handling for generated thumbnails.

package connector

import (
	"encoding/binary"
	"image"
)

// EXIF orientation values (TIFF tag 0x0112). 1 is upright; 2/4/5/7 include a
// mirror, 3/6/8 are pure rotations.
const (
	orientationNormal = 1
	orientationMax    = 8
)

// exifOrientation returns the EXIF orientation of a JPEG, or 1 when there is
// none to read.
//
// Go's image/jpeg decoder ignores EXIF entirely and jpeg.Encode writes none, so
// a re-encoded thumbnail loses the tag that told the client how to rotate the
// original. The full-size image keeps it — the bytes are passed through, and the
// HEIC path deliberately re-embeds the tag when converting (see
// writeJPEGWithMetadata) — which is why only thumbnails come out sideways.
//
// Anything unparseable returns 1 rather than an error: a thumbnail that is not
// rotated is the current behaviour, so failing to read the tag can only leave
// things as they are, never make them worse.
func exifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return orientationNormal // not a JPEG
	}
	// Walk the segment chain looking for APP1/Exif.
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return orientationNormal
		}
		marker := data[i+1]
		// Start of scan or end of image: no more metadata segments follow.
		if marker == 0xDA || marker == 0xD9 {
			return orientationNormal
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return orientationNormal
		}
		if marker == 0xE1 {
			payload := data[i+4 : i+2+segLen]
			const exifHeader = "Exif\x00\x00"
			if len(payload) > len(exifHeader) && string(payload[:len(exifHeader)]) == exifHeader {
				return tiffOrientation(payload[len(exifHeader):])
			}
		}
		i += 2 + segLen
	}
	return orientationNormal
}

// tiffOrientation reads tag 0x0112 out of a raw TIFF/EXIF block — the form
// libheif hands back for HEIC, and the payload of a JPEG APP1/Exif segment
// once its "Exif\0\0" header is stripped.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return orientationNormal
	}
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return orientationNormal
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return orientationNormal
	}
	ifd := int(bo.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return orientationNormal
	}
	count := int(bo.Uint16(tiff[ifd : ifd+2]))
	for i := range count {
		off := ifd + 2 + i*12
		if off+12 > len(tiff) {
			return orientationNormal
		}
		if bo.Uint16(tiff[off:off+2]) != 0x0112 {
			continue
		}
		// SHORT value, stored inline in the entry's value field.
		v := int(bo.Uint16(tiff[off+8 : off+10]))
		if v < orientationNormal || v > orientationMax {
			return orientationNormal
		}
		return v
	}
	return orientationNormal
}

// orientationSwapsAxes reports whether an orientation exchanges width and
// height — the 90° cases, where the decoded bounds are transposed relative to
// how the image is meant to be displayed.
func orientationSwapsAxes(orientation int) bool {
	switch orientation {
	case 5, 6, 7, 8:
		return true
	default:
		return false
	}
}

// displayDims converts decoded pixel dimensions into the dimensions the image
// is meant to be shown at.
//
// This matters beyond the thumbnail: Info.Width/Height are taken from the
// decoded bounds, so for a portrait photo shot on a phone (orientation 6, the
// most common non-upright value) clients were told the image was landscape and
// reserved the wrong shape for it.
func displayDims(w, h, orientation int) (int, int) {
	if orientationSwapsAxes(orientation) {
		return h, w
	}
	return w, h
}

// orientedSourcePixel maps a pixel in the display-oriented output back to its
// coordinate in the decoded source, for an image whose EXIF says it needs
// transforming. dstW and dstH are in display orientation.
//
// The thumbnail is rotated in pixels rather than by copying the EXIF tag
// forward: a thumbnail that is already upright renders correctly in every
// client whether or not it reads EXIF, and it avoids embedding the original's
// metadata (which describes the full-size image) into a scaled-down copy.
func orientedSourcePixel(x, y, dstW, dstH, orientation int) (int, int) {
	switch orientation {
	case 2: // mirror horizontal
		return dstW - 1 - x, y
	case 3: // rotate 180
		return dstW - 1 - x, dstH - 1 - y
	case 4: // mirror vertical
		return x, dstH - 1 - y
	case 5: // mirror horizontal, rotate 270 CW
		return y, x
	case 6: // rotate 90 CW
		return y, dstW - 1 - x
	case 7: // mirror horizontal, rotate 90 CW
		return dstH - 1 - y, dstW - 1 - x
	case 8: // rotate 270 CW
		return dstH - 1 - y, x
	default: // 1, and anything unrecognised
		return x, y
	}
}

// orientImage returns img transformed so it is upright, or img unchanged when
// the orientation is normal.
func orientImage(img image.Image, orientation int) image.Image {
	if orientation <= orientationNormal || orientation > orientationMax {
		return img
	}
	src := img.Bounds()
	dstW, dstH := displayDims(src.Dx(), src.Dy(), orientation)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := range dstH {
		for x := range dstW {
			sx, sy := orientedSourcePixel(x, y, dstW, dstH, orientation)
			dst.Set(x, y, img.At(src.Min.X+sx, src.Min.Y+sy))
		}
	}
	return dst
}
