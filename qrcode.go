// go-qrcode
// Copyright 2014 Tom Harwood

/*
Package qrcode implements a QR Code encoder.

A QR Code is a matrix (two-dimensional) barcode. Arbitrary content may be
encoded.

A QR Code contains error recovery information to aid reading damaged or
obscured codes. There are four levels of error recovery: qrcode.{Low, Medium,
High, Highest}. QR Codes with a higher recovery level are more robust to damage,
at the cost of being physically larger.

Three functions cover most use cases:

- Create a PNG image:

	var png []byte
	png, err := qrcode.Encode("https://example.org", qrcode.Medium, 256)

- Create a PNG image and write to a file:

	err := qrcode.WriteFile("https://example.org", qrcode.Medium, 256, "qr.png")

- Create a PNG image with custom colors and write to file:

	err := qrcode.WriteColorFile("https://example.org", qrcode.Medium, 256, color.Black, color.White, "qr.png")

All examples use the qrcode.Medium error Recovery Level and create a fixed
256x256px size QR Code. The last function creates a white on black instead of black
on white QR Code.

To generate a variable sized image instead, specify a negative size (in place of
the 256 above), such as -4 or -5. Larger negative numbers create larger images:
A size of -5 sets each module (QR Code "pixel") to be 5px wide/high.

- Create a PNG image (variable size, with minimum white padding) and write to a file:

	err := qrcode.WriteFile("https://example.org", qrcode.Medium, -5, "qr.png")

The maximum capacity of a QR Code varies according to the content encoded and
the error recovery level. The maximum capacity is 2,953 bytes, 4,296
alphanumeric characters, 7,089 numeric digits, or a combination of these.

This package implements a subset of QR Code 2005, as defined in ISO/IEC
18004:2006.
*/
package qrcode

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"

	"github.com/disintegration/imaging"

	bitset "github.com/skip2/go-qrcode/bitset"
	"github.com/skip2/go-qrcode/reedsolomon"
)

// Encode a QR Code and return a raw PNG image.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently returned. Negative values for size cause a
// variable sized image to be returned: See the documentation for Image().
//
// To serve over HTTP, remember to send a Content-Type: image/png header.
func Encode(content string, level RecoveryLevel, size int) ([]byte, error) {
	var q *QRCode

	q, err := New(content, level)

	if err != nil {
		return nil, err
	}

	return q.PNG(size)
}

// WriteFile encodes, then writes a QR Code to the given filename in PNG format.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently written. Negative values for size cause a variable
// sized image to be written: See the documentation for Image().
func WriteFile(content string, level RecoveryLevel, size int, filename string) error {
	var q *QRCode

	q, err := New(content, level)

	if err != nil {
		return err
	}

	return q.WriteFile(size, filename)
}

// WriteColorFile encodes, then writes a QR Code to the given filename in PNG format.
// With WriteColorFile you can also specify the colors you want to use.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently written. Negative values for size cause a variable
// sized image to be written: See the documentation for Image().
func WriteColorFile(content string, level RecoveryLevel, size int, background,
	foreground color.Color, filename string) error {

	var q *QRCode

	q, err := New(content, level)

	q.BackgroundColor = background
	q.BoxColor = foreground
	q.PixelColor = foreground

	if err != nil {
		return err
	}

	return q.WriteFile(size, filename)
}

// A QRCode represents a valid encoded QRCode.
type QRCode struct {
	// Original content encoded.
	Content string

	// QR Code type.
	Level         RecoveryLevel
	VersionNumber int

	// User settable drawing options.
	centerLogoBackgroundOffset int
	CenterLogo                 *image.Image
	FinderPatternImage         *image.Image
	AlignmentPatternImage      *image.Image
	BackgroundColor            color.Color
	BoxColor                   color.Color
	PixelColor                 color.Color

	// Disable the QR Code border.
	DisableBorder bool

	encoder *dataEncoder
	version qrCodeVersion

	data   *bitset.Bitset
	symbol *symbol
	mask   int

	// cache for logo sizing
	centerLogoCache            map[int]image.Image
	finderPatternImageCache    map[int]image.Image
	alignmentPatternImageCache map[int]image.Image
}

// New constructs a QRCode.
//
//	var q *qrcode.QRCode
//	q, err := qrcode.New("my content", qrcode.Medium)
//
// An error occurs if the content is too long.
func New(content string, level RecoveryLevel) (*QRCode, error) {
	encoders := []dataEncoderType{dataEncoderType1To9, dataEncoderType10To26,
		dataEncoderType27To40}

	var encoder *dataEncoder
	var encoded *bitset.Bitset
	var chosenVersion *qrCodeVersion
	var err error

	for _, t := range encoders {
		encoder = newDataEncoder(t)
		encoded, err = encoder.encode([]byte(content))

		if err != nil {
			continue
		}

		chosenVersion = chooseQRCodeVersion(level, encoder, encoded.Len())

		if chosenVersion != nil {
			break
		}
	}

	if err != nil {
		return nil, err
	} else if chosenVersion == nil {
		return nil, errors.New("content too long to encode")
	}

	q := &QRCode{
		Content: content,

		Level:         level,
		VersionNumber: chosenVersion.version,

		BackgroundColor: color.White,
		PixelColor:      color.Black,
		BoxColor:        color.Black,

		encoder: encoder,
		data:    encoded,
		version: *chosenVersion,
	}

	return q, nil
}

// NewWithForcedVersion constructs a QRCode of a specific version.
//
//	var q *qrcode.QRCode
//	q, err := qrcode.NewWithForcedVersion("my content", 25, qrcode.Medium)
//
// An error occurs in case of invalid version.
func NewWithForcedVersion(content string, version int, level RecoveryLevel) (*QRCode, error) {
	var encoder *dataEncoder

	switch {
	case version >= 1 && version <= 9:
		encoder = newDataEncoder(dataEncoderType1To9)
	case version >= 10 && version <= 26:
		encoder = newDataEncoder(dataEncoderType10To26)
	case version >= 27 && version <= 40:
		encoder = newDataEncoder(dataEncoderType27To40)
	default:
		return nil, fmt.Errorf("Invalid version %d (expected 1-40 inclusive)", version)
	}

	var encoded *bitset.Bitset
	encoded, err := encoder.encode([]byte(content))

	if err != nil {
		return nil, err
	}

	chosenVersion := getQRCodeVersion(level, version)

	if chosenVersion == nil {
		return nil, errors.New("cannot find QR Code version")
	}

	if encoded.Len() > chosenVersion.numDataBits() {
		return nil, fmt.Errorf("Cannot encode QR code: content too large for fixed size QR Code version %d (encoded length is %d bits, maximum length is %d bits)",
			version,
			encoded.Len(),
			chosenVersion.numDataBits())
	}

	q := &QRCode{
		Content: content,

		Level:         level,
		VersionNumber: chosenVersion.version,

		BackgroundColor: color.White,
		PixelColor:      color.Black,
		BoxColor:        color.Black,

		encoder: encoder,
		data:    encoded,
		version: *chosenVersion,
	}

	return q, nil
}

// NewWithMinimumVersion constructs a QRCode with a minimum version. This should be used when generating custom QRCode for higher error recovery.
//
// var q *qrcode.QRCode
// q, err := qrcode.NewWithMinimumVersion("my content", 6, grcode.Highest)
//
// An error occurs if the content is too long.
func NewWithMinimumVersion(content string, minVersion int, level RecoveryLevel) (*QRCode, error) {
	code, err := New(content, level)
	if err != nil {
		return nil, err
	}

	if code.VersionNumber >= minVersion {
		return code, nil
	}

	return NewWithForcedVersion(content, minVersion, level)
}

// Bitmap returns the QR Code as a 2D array of 1-bit pixels.
//
// bitmap[y][x] is true if the pixel at (x, y) is set.
//
// The bitmap includes the required "quiet zone" around the QR Code to aid
// decoding.
func (q *QRCode) Bitmap() [][]bool {
	// Build QR code.
	q.encode()

	return q.symbol.bitmap()
}

// Image returns the QR Code as an image.Image.
//
// A positive size sets a fixed image width and height (e.g. 256 yields an
// 256x256px image).
//
// Depending on the amount of data encoded, fixed size images can have different
// amounts of padding (white space around the QR Code). As an alternative, a
// variable sized image can be generated instead:
//
// A negative size causes a variable sized image to be returned. The image
// returned is the minimum size required for the QR Code. Choose a larger
// negative number to increase the scale of the image. e.g. a size of -5 causes
// each module (QR Code "pixel") to be 5px in size.
func (q *QRCode) Image(size int) image.Image {
	// Build QR code.
	q.encode()

	// Minimum pixels (both width and height) required.
	realSize := q.symbol.size

	// Variable size support.
	if size < 0 {
		size = size * -1 * realSize
	}

	// Actual pixels available to draw the symbol. Automatically increase the
	// image size if it's not large enough.
	if size < realSize {
		size = realSize
	}

	// Output image.
	rect := image.Rectangle{Min: image.Point{0, 0}, Max: image.Point{size, size}}

	// Saves a few bytes to have them in this order
	p := color.Palette([]color.Color{q.BackgroundColor, q.BoxColor, q.PixelColor})
	img := image.NewPaletted(rect, p)

	// Map each image pixel to the nearest QR code module.
	modulesPerPixel := float64(realSize) / float64(size)

	// QR code bitmap.
	bitmap := q.symbol.bitmap()

	// color pixels
	fgClr := uint8(img.Palette.Index(q.PixelColor))
	for y := 0; y < size; y++ {
		y2 := int(float64(y) * modulesPerPixel)
		for x := 0; x < size; x++ {
			x2 := int(float64(x) * modulesPerPixel)

			v := bitmap[y2][x2]

			if v {
				pos := img.PixOffset(x, y)
				img.Pix[pos] = fgClr
			}
		}
	}

	// QR code boxes map.
	boxes := q.symbol.finderPatternBitmap()

	// color boxes
	fgClr = uint8(img.Palette.Index(q.BoxColor))
	for y := 0; y < size; y++ {
		y2 := int(float64(y) * modulesPerPixel)
		for x := 0; x < size; x++ {
			x2 := int(float64(x) * modulesPerPixel)

			v := boxes[y2][x2]

			if v {
				pos := img.PixOffset(x, y)
				img.Pix[pos] = fgClr
			}
		}
	}

	return img
}

// BeautifyImage returns the QR Code as an image.Image.
//
// A positive size sets a fixed image width and height (e.g. 256 yields an
// 256x256px image).
//
// Depending on the amount of data encoded, fixed size images can have different
// amounts of padding (white space around the QR Code). As an alternative, a
// variable sized image can be generated instead:
//
// A negative size causes a variable sized image to be returned. The image
// returned is the minimum size required for the QR Code. Choose a larger
// negative number to increase the scale of the image. e.g. a size of -5 causes
// each module (QR Code "pixel") to be 5px in size.
func (q *QRCode) BeautifyImage(size int) image.Image {
	// Build QR code.
	q.encode()

	// Minimum pixels (both width and height) required.
	realSize := q.symbol.size

	// Variable size support.
	if size < 0 {
		size = size * -1 * realSize
	}

	// Actual pixels available to draw the symbol. Automatically increase the
	// image size if it's not large enough.
	if size < realSize {
		size = realSize
	}

	// Output image.
	rect := image.Rectangle{Min: image.Point{0, 0}, Max: image.Point{size, size}}

	// Saves a few bytes to have them in this order
	img := image.NewRGBA(rect)

	// set everything to background color
	for x := 0; x < img.Bounds().Max.X; x++ {
		for y := 0; y < img.Bounds().Max.Y; y++ {
			img.Set(x, y, q.BackgroundColor)

		}
	}

	// Map each image pixel to the nearest QR code module.
	modulesPerPixel := float64(realSize) / float64(size)
	sizePerPoint := int(float64(size) / float64(realSize))

	// undrawables
	finderPatternMap := make(map[string]struct{})
	alignmentPatternMap := make(map[string]struct{})
	logoMap := make(map[string]struct{})

	// QR code finder pattern bitmap.
	bitmap := q.symbol.finderPatternBitmap()
	for y := 0; y < size; y++ {
		y2 := int(float64(y) * modulesPerPixel)
		for x := 0; x < size; x++ {
			x2 := int(float64(x) * modulesPerPixel)

			v := bitmap[y2][x2]

			if v {
				pixel := fmt.Sprintf("%d,%d", y2, x2)
				finderPatternMap[pixel] = struct{}{}
			}
		}
	}

	if q.FinderPatternImage != nil {
		if q.finderPatternImageCache == nil {
			q.finderPatternImageCache = make(map[int]image.Image)
		}

		box := *q.FinderPatternImage
		boxSize := int(float64(q.symbol.finderPatternSize) / modulesPerPixel)
		var boxFit image.Image
		if fittedBox, found := q.finderPatternImageCache[boxSize]; found {
			boxFit = fittedBox
		} else {
			boxFitted := imaging.Fit(box, boxSize, boxSize, imaging.Lanczos)
			boxBackground := rectangleImage(boxSize, boxSize, q.BackgroundColor)
			boxFit = overlayImages(boxBackground, boxFitted, image.Point{})
			q.finderPatternImageCache[boxSize] = boxFit
		}

		borderSize := q.symbol.borderSize()
		TLMin, TRMin, BLMin := q.symbol.finderPatternPoints()

		TLMin.X = int(float64(TLMin.X+borderSize) / modulesPerPixel)
		TLMin.Y = int(float64(TLMin.Y+borderSize) / modulesPerPixel)

		TRMin.X = int(float64(TRMin.X+borderSize) / modulesPerPixel)
		TRMin.Y = int(float64(TRMin.Y+borderSize) / modulesPerPixel)

		BLMin.X = int(float64(BLMin.X+borderSize) / modulesPerPixel)
		BLMin.Y = int(float64(BLMin.Y+borderSize) / modulesPerPixel)

		TLMax := TLMin.Add(image.Point{boxSize, boxSize})
		TRMax := TRMin.Add(image.Point{boxSize, boxSize})
		BLMax := BLMin.Add(image.Point{boxSize, boxSize})

		for x := TLMin.X; x < TLMax.X; x++ {
			for y := TLMin.Y; y < TLMax.Y; y++ {
				img.Set(x, y, boxFit.At(x-TLMin.X, y-TLMin.Y))
			}
		}

		for x := TRMin.X; x < TRMax.X; x++ {
			for y := TRMin.Y; y < TRMax.Y; y++ {
				img.Set(x, y, boxFit.At(x-TRMin.X, y-TRMin.Y))
			}
		}

		for x := BLMin.X; x < BLMax.X; x++ {
			for y := BLMin.Y; y < BLMax.Y; y++ {
				img.Set(x, y, boxFit.At(x-BLMin.X, y-BLMin.Y))
			}
		}

	} else {

		for x := 0; x < realSize; x++ {
			for y := 0; y < realSize; y++ {
				if bitmap[y][x] {

					// find the box of pixels to light up
					minX, minY := int(math.Round(float64(x)/modulesPerPixel)), int(math.Round(float64(y)/modulesPerPixel))
					maxX, maxY := minX+sizePerPoint, minY+sizePerPoint

					for xp := minX; xp < maxX; xp++ {
						for yp := minY; yp < maxY; yp++ {
							img.Set(xp, yp, q.BoxColor)
						}
					}
				}
			}
		}
	}

	// QR code last alignment pattern bitmap.
	bitmap = q.symbol.lastAlignmentPatternBitmap()
	for y := 0; y < size; y++ {
		y2 := int(float64(y) * modulesPerPixel)
		for x := 0; x < size; x++ {
			x2 := int(float64(x) * modulesPerPixel)

			v := bitmap[y2][x2]

			if v {
				pixel := fmt.Sprintf("%d,%d", y2, x2)
				alignmentPatternMap[pixel] = struct{}{}
			}
		}
	}

	if q.AlignmentPatternImage != nil {
		if q.alignmentPatternImageCache == nil {
			q.alignmentPatternImageCache = make(map[int]image.Image)
		}

		box := *q.AlignmentPatternImage
		boxSize := int(float64(q.symbol.alignmentPatternSize) / modulesPerPixel)
		var boxFit image.Image
		if fittedBox, found := q.alignmentPatternImageCache[boxSize]; found {
			boxFit = fittedBox
		} else {
			boxFitted := imaging.Fit(box, boxSize, boxSize, imaging.Lanczos)
			boxBackground := rectangleImage(boxSize, boxSize, q.BackgroundColor)
			boxFit = overlayImages(boxBackground, boxFitted, image.Point{})
			q.alignmentPatternImageCache[boxSize] = boxFit
		}

		borderSize := q.symbol.borderSize()
		minPt := q.symbol.alignmentPatternPoint

		minPt.X = int(float64(minPt.X+borderSize) / modulesPerPixel)
		minPt.Y = int(float64(minPt.Y+borderSize) / modulesPerPixel)

		maxPt := minPt.Add(image.Point{boxSize, boxSize})

		for x := minPt.X; x < maxPt.X; x++ {
			for y := minPt.Y; y < maxPt.Y; y++ {
				img.Set(x, y, boxFit.At(x-minPt.X, y-minPt.Y))
			}
		}

	} else {

		for x := 0; x < realSize; x++ {
			for y := 0; y < realSize; y++ {
				if bitmap[y][x] {

					// find the box of pixels to light up
					minX, minY := int(math.Round(float64(x)/modulesPerPixel)), int(math.Round(float64(y)/modulesPerPixel))
					maxX, maxY := minX+sizePerPoint, minY+sizePerPoint

					for xp := minX; xp < maxX; xp++ {
						for yp := minY; yp < maxY; yp++ {
							img.Set(xp, yp, q.PixelColor)
						}
					}
				}
			}
		}
	}

	if q.CenterLogo != nil {
		if q.centerLogoCache == nil {
			q.centerLogoCache = make(map[int]image.Image)
		}

		logo := *q.CenterLogo
		maxLogoSize := int(float64(size) * 0.35)
		logoSize := logo.Bounds().Max.X
		if logo.Bounds().Max.Y > logoSize {
			logoSize = logo.Bounds().Max.Y
		}
		if logoSize > maxLogoSize {
			logoSize = maxLogoSize
		}
		if logoSize%2 != 0 {
			logoSize -= 1
		}

		var logoFit image.Image

		if fittedLogo, found := q.centerLogoCache[logoSize]; found {
			logoFit = fittedLogo
		} else {
			logoFitted := imaging.Fit(logo, logoSize, logoSize, imaging.Lanczos)
			logoBackground := circleImage(logoSize/2+q.centerLogoBackgroundOffset, q.BackgroundColor)
			logoFit = overlayImages(logoBackground, logoFitted, image.Point{-q.centerLogoBackgroundOffset, -q.centerLogoBackgroundOffset})
			q.centerLogoCache[logoSize] = logoFit
		}

		minX := (size - logoFit.Bounds().Max.X) / 2
		minY := (size - logoFit.Bounds().Max.Y) / 2
		maxX := minX + logoFit.Bounds().Max.X
		maxY := minY + logoFit.Bounds().Max.Y

		for x := minX; x < maxX; x++ {
			for y := minY; y < maxY; y++ {
				r, g, b, a := logoFit.At(x-minX, y-minY).RGBA()
				rI, gI, bI, aI := img.At(x, y).RGBA()
				newCol := color.NRGBA{
					R: uint8(r + rI),
					G: uint8(g + gI),
					B: uint8(b + bI),
					A: uint8(a + aI),
				}

				if a == 65535 {
					y2 := int(float64(y) * modulesPerPixel)
					x2 := int(float64(x) * modulesPerPixel)
					pixel := fmt.Sprintf("%d,%d", y2, x2)
					logoMap[pixel] = struct{}{}
					img.Set(x, y, newCol)
				}
			}
		}

	}

	// QR code bitmap.
	bitmap = q.symbol.bitmap()
	for x := 0; x < realSize; x++ {
		for y := 0; y < realSize; y++ {
			if bitmap[y][x] {
				pixel := fmt.Sprintf("%d,%d", y, x)
				if _, found := finderPatternMap[pixel]; !found {
					if _, found = alignmentPatternMap[pixel]; !found {
						if _, found = logoMap[pixel]; !found {

							// find the box of pixels to light up
							minX, minY := int(math.Round(float64(x)/modulesPerPixel)), int(math.Round(float64(y)/modulesPerPixel))
							maxX, maxY := minX+sizePerPoint, minY+sizePerPoint

							for xp := minX; xp < maxX; xp++ {
								for yp := minY; yp < maxY; yp++ {
									img.Set(xp, yp, q.PixelColor)
								}
							}

						}
					}
				}
			}
		}
	}

	return img
}

func (q *QRCode) LoadAndSetCenterLogo(path string, offset int) error {
	img, err := loadImage(path)
	if err == nil {
		q.CenterLogo = img
		q.centerLogoBackgroundOffset = offset
	}
	return err
}

func (q *QRCode) LoadAndSetFinderPatternImage(path string) error {
	img, err := loadImage(path)
	if err == nil {
		q.FinderPatternImage = img
	}
	return err
}

func (q *QRCode) LoadAndSetAlignmentPatternImage(path string) error {
	img, err := loadImage(path)
	if err == nil {
		q.AlignmentPatternImage = img
	}
	return err
}

func loadImage(path string) (*image.Image, error) {
	file, openErr := os.Open(path)
	if openErr != nil {
		return nil, openErr
	}

	logo, _, decodeErr := image.Decode(file)
	if decodeErr != nil {
		return nil, decodeErr
	}

	return &logo, nil
}

func overlayImages(base, overlay image.Image, offset image.Point) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, base.Bounds().Max.X, base.Bounds().Max.Y))

	draw.Draw(img, img.Bounds(), base, image.Point{}, draw.Src)
	draw.Draw(img, overlay.Bounds(), overlay, offset, draw.Over)

	return img
}

type circleStruct struct {
	p image.Point
	r int
	c color.Color
}

func (c *circleStruct) ColorModel() color.Model {
	return color.RGBAModel
}

func (c *circleStruct) Bounds() image.Rectangle {
	return image.Rect(c.p.X-int(c.r), c.p.Y-int(c.r), c.p.X+int(c.r), c.p.Y+int(c.r))
}

func (c *circleStruct) At(x, y int) color.Color {
	xx, yy, rr := float64(x-c.p.X)+0.5, float64(y-c.p.Y)+0.5, float64(c.r)
	if xx*xx+yy*yy < rr*rr {
		return c.c
	}
	return color.Alpha{0}
}

func circleImage(radius int, c color.Color) image.Image {
	return &circleStruct{p: image.Point{radius, radius}, r: radius, c: c}
}

type rectangleStruct struct {
	size image.Point
	c    color.Color
}

func (r *rectangleStruct) ColorModel() color.Model {
	return color.RGBAModel
}

func (r *rectangleStruct) Bounds() image.Rectangle {
	return image.Rect(0, 0, r.size.X, r.size.Y)
}

func (r *rectangleStruct) At(x, y int) color.Color {
	return r.c
}

func rectangleImage(width, height int, c color.Color) image.Image {
	return &rectangleStruct{size: image.Point{width, height}, c: c}
}

// PNG returns the QR Code as a PNG image.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently returned. Negative values for size cause a
// variable sized image to be returned: See the documentation for Image().
func (q *QRCode) PNG(size int) ([]byte, error) {
	img := q.Image(size)

	encoder := png.Encoder{CompressionLevel: png.BestCompression}

	var b bytes.Buffer
	err := encoder.Encode(&b, img)

	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// Write writes the QR Code as a PNG image to io.Writer.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently written. Negative values for size cause a
// variable sized image to be written: See the documentation for Image().
func (q *QRCode) Write(size int, out io.Writer) error {
	var png []byte

	png, err := q.PNG(size)

	if err != nil {
		return err
	}
	_, err = out.Write(png)
	return err
}

// WriteFile writes the QR Code as a PNG image to the specified file.
//
// size is both the image width and height in pixels. If size is too small then
// a larger image is silently written. Negative values for size cause a
// variable sized image to be written: See the documentation for Image().
func (q *QRCode) WriteFile(size int, filename string) error {
	var png []byte

	png, err := q.PNG(size)

	if err != nil {
		return err
	}

	return ioutil.WriteFile(filename, png, os.FileMode(0644))
}

// encode completes the steps required to encode the QR Code. These include
// adding the terminator bits and padding, splitting the data into blocks and
// applying the error correction, and selecting the best data mask.
func (q *QRCode) encode() {
	numTerminatorBits := q.version.numTerminatorBitsRequired(q.data.Len())

	q.addTerminatorBits(numTerminatorBits)
	q.addPadding()

	encoded := q.encodeBlocks()

	const numMasks int = 8
	penalty := 0

	for mask := 0; mask < numMasks; mask++ {
		var s *symbol
		var err error

		s, err = buildRegularSymbol(q.version, mask, encoded, !q.DisableBorder)

		if err != nil {
			log.Panic(err.Error())
		}

		numEmptyModules := s.numEmptyModules()
		if numEmptyModules != 0 {
			log.Panicf("bug: numEmptyModules is %d (expected 0) (version=%d)",
				numEmptyModules, q.VersionNumber)
		}

		p := s.penaltyScore()

		// log.Printf("mask=%d p=%3d p1=%3d p2=%3d p3=%3d p4=%d\n", mask, p, s.penalty1(), s.penalty2(), s.penalty3(), s.penalty4())

		if q.symbol == nil || p < penalty {
			q.symbol = s
			q.mask = mask
			penalty = p
		}
	}
}

// addTerminatorBits adds final terminator bits to the encoded data.
//
// The number of terminator bits required is determined when the QR Code version
// is chosen (which itself depends on the length of the data encoded). The
// terminator bits are thus added after the QR Code version
// is chosen, rather than at the data encoding stage.
func (q *QRCode) addTerminatorBits(numTerminatorBits int) {
	q.data.AppendNumBools(numTerminatorBits, false)
}

// encodeBlocks takes the completed (terminated & padded) encoded data, splits
// the data into blocks (as specified by the QR Code version), applies error
// correction to each block, then interleaves the blocks together.
//
// The QR Code's final data sequence is returned.
func (q *QRCode) encodeBlocks() *bitset.Bitset {
	// Split into blocks.
	type dataBlock struct {
		data          *bitset.Bitset
		ecStartOffset int
	}

	block := make([]dataBlock, q.version.numBlocks())

	start := 0
	end := 0
	blockID := 0

	for _, b := range q.version.block {
		for j := 0; j < b.numBlocks; j++ {
			start = end
			end = start + b.numDataCodewords*8

			// Apply error correction to each block.
			numErrorCodewords := b.numCodewords - b.numDataCodewords
			block[blockID].data = reedsolomon.Encode(q.data.Substr(start, end), numErrorCodewords)
			block[blockID].ecStartOffset = end - start

			blockID++
		}
	}

	// Interleave the blocks.

	result := bitset.New()

	// Combine data blocks.
	working := true
	for i := 0; working; i += 8 {
		working = false

		for j, b := range block {
			if i >= block[j].ecStartOffset {
				continue
			}

			result.Append(b.data.Substr(i, i+8))

			working = true
		}
	}

	// Combine error correction blocks.
	working = true
	for i := 0; working; i += 8 {
		working = false

		for j, b := range block {
			offset := i + block[j].ecStartOffset
			if offset >= block[j].data.Len() {
				continue
			}

			result.Append(b.data.Substr(offset, offset+8))

			working = true
		}
	}

	// Append remainder bits.
	result.AppendNumBools(q.version.numRemainderBits, false)

	return result
}

// max returns the maximum of a and b.
func max(a int, b int) int {
	if a > b {
		return a
	}

	return b
}

// addPadding pads the encoded data upto the full length required.
func (q *QRCode) addPadding() {
	numDataBits := q.version.numDataBits()

	if q.data.Len() == numDataBits {
		return
	}

	// Pad to the nearest codeword boundary.
	q.data.AppendNumBools(q.version.numBitsToPadToCodeword(q.data.Len()), false)

	// Pad codewords 0b11101100 and 0b00010001.
	padding := [2]*bitset.Bitset{
		bitset.New(true, true, true, false, true, true, false, false),
		bitset.New(false, false, false, true, false, false, false, true),
	}

	// Insert pad codewords alternately.
	i := 0
	for numDataBits-q.data.Len() >= 8 {
		q.data.Append(padding[i])

		i = 1 - i // Alternate between 0 and 1.
	}

	if q.data.Len() != numDataBits {
		log.Panicf("BUG: got len %d, expected %d", q.data.Len(), numDataBits)
	}
}

// ToString produces a multi-line string that forms a QR-code image.
func (q *QRCode) ToString(inverseColor bool) string {
	bits := q.Bitmap()
	var buf bytes.Buffer
	for y := range bits {
		for x := range bits[y] {
			if bits[y][x] != inverseColor {
				buf.WriteString("  ")
			} else {
				buf.WriteString("██")
			}
		}
		buf.WriteString("\n")
	}
	return buf.String()
}

// ToSmallString produces a multi-line string that forms a QR-code image, a
// factor two smaller in x and y then ToString.
func (q *QRCode) ToSmallString(inverseColor bool) string {
	bits := q.Bitmap()
	var buf bytes.Buffer
	// if there is an odd number of rows, the last one needs special treatment
	for y := 0; y < len(bits)-1; y += 2 {
		for x := range bits[y] {
			if bits[y][x] == bits[y+1][x] {
				if bits[y][x] != inverseColor {
					buf.WriteString(" ")
				} else {
					buf.WriteString("█")
				}
			} else {
				if bits[y][x] != inverseColor {
					buf.WriteString("▄")
				} else {
					buf.WriteString("▀")
				}
			}
		}
		buf.WriteString("\n")
	}
	// special treatment for the last row if odd
	if len(bits)%2 == 1 {
		y := len(bits) - 1
		for x := range bits[y] {
			if bits[y][x] != inverseColor {
				buf.WriteString(" ")
			} else {
				buf.WriteString("▀")
			}
		}
		buf.WriteString("\n")
	}
	return buf.String()
}
