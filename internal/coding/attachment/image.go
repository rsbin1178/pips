package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
)

const (
	imageFormatGIF          = "gif"
	imageFormatJPEG         = "jpeg"
	imageFormatPNG          = "png"
	normalizedJPEGMediaType = "image/jpeg"
)

type decodedImage struct {
	image  image.Image
	format string
	width  int
	height int
}

type normalizationStep struct {
	maxEdge int
	quality int
}

var normalizationSteps = [...]normalizationStep{
	{maxEdge: 2048, quality: 85},
	{maxEdge: 1600, quality: 82},
	{maxEdge: 1280, quality: 78},
	{maxEdge: 1024, quality: 72},
	{maxEdge: 768, quality: 65},
	{maxEdge: 512, quality: 55},
}

// Image is immutable normalized inline image data. Byte-returning methods
// detach their result so callers cannot mutate retained Composer snapshots.
type Image struct {
	data     []byte
	mimeType string
	name     string
	width    int
	height   int
	digest   [sha256.Size]byte
}

// NormalizeImage validates and normalizes one encoded PNG, JPEG, or GIF.
// GIF input uses only its first frame. Valid PNG/JPEG input at or below the
// inline limit is preserved byte-for-byte; all other accepted input follows a
// fixed deterministic resize/JPEG sequence.
func NormalizeImage(name string, encoded []byte) (Image, error) {
	return NormalizeImageContext(context.Background(), name, encoded)
}

// NormalizeImageContext is NormalizeImage with cancellation checks around
// decode and throughout bounded resize work.
func NormalizeImageContext(ctx context.Context, name string, encoded []byte) (Image, error) {
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}

	if err := validateImageInput(name, encoded); err != nil {
		return Image{}, err
	}

	decoded, err := decodeValidatedImage(ctx, encoded)
	if err != nil {
		return Image{}, err
	}

	if mimeType, preserve := preservedImageMediaType(decoded.format, len(encoded)); preserve {
		return newImage(name, mimeType, decoded.width, decoded.height, encoded), nil
	}

	return encodeNormalizedImage(ctx, name, decoded)
}

func validateImageInput(name string, encoded []byte) error {
	if err := validateImageName(name); err != nil {
		return err
	}

	if len(encoded) == 0 || len(encoded) > MaxEncodedImageBytes {
		return fmt.Errorf("%w: encoded image exceeds ingress limit", ErrLimit)
	}

	return nil
}

func decodeValidatedImage(ctx context.Context, encoded []byte) (decodedImage, error) {
	configuration, format, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		if unsupportedImageSignature(encoded) {
			return decodedImage{}, ErrUnsupportedImage
		}

		return decodedImage{}, fmt.Errorf("%w: decode configuration", ErrInvalidImage)
	}

	if !supportedImageFormat(format) {
		return decodedImage{}, fmt.Errorf("%w: %s", ErrUnsupportedImage, format)
	}

	if err := validateImageDimensions(configuration.Width, configuration.Height); err != nil {
		return decodedImage{}, err
	}

	if err := ctx.Err(); err != nil {
		return decodedImage{}, err
	}

	decoded, err := decodeImageFirstFrame(format, encoded)
	if err != nil {
		return decodedImage{}, fmt.Errorf("%w: decode payload", ErrInvalidImage)
	}

	bounds := decoded.Bounds()
	if bounds.Dx() != configuration.Width || bounds.Dy() != configuration.Height {
		return decodedImage{}, fmt.Errorf("%w: dimensions changed during decode", ErrInvalidImage)
	}

	if err := ctx.Err(); err != nil {
		return decodedImage{}, err
	}

	return decodedImage{
		image: decoded, format: format,
		width: configuration.Width, height: configuration.Height,
	}, nil
}

func supportedImageFormat(format string) bool {
	return format == imageFormatPNG || format == imageFormatJPEG || format == imageFormatGIF
}

func preservedImageMediaType(format string, size int) (string, bool) {
	if size > MaxImageBytes {
		return "", false
	}

	switch format {
	case imageFormatPNG:
		return "image/png", true
	case imageFormatJPEG:
		return normalizedJPEGMediaType, true
	default:
		return "", false
	}
}

func encodeNormalizedImage(ctx context.Context, name string, decoded decodedImage) (Image, error) {
	for _, step := range normalizationSteps {
		width, height := fittedDimensions(decoded.width, decoded.height, step.maxEdge)

		resized, err := resizeOnWhite(ctx, decoded.image, width, height)
		if err != nil {
			return Image{}, err
		}

		var output bytes.Buffer
		if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: step.quality}); err != nil {
			return Image{}, fmt.Errorf("%w: encode normalized JPEG", ErrInvalidImage)
		}

		if output.Len() <= MaxImageBytes {
			return newImage(name, normalizedJPEGMediaType, width, height, output.Bytes()), nil
		}
	}

	return Image{}, fmt.Errorf("%w: normalized image exceeds inline limit", ErrLimit)
}

// Clone returns a fully detached normalized image.
func (i Image) Clone() Image {
	i.data = slices.Clone(i.data)

	return i
}

// Valid reports whether the value was produced by NormalizeImage and remains
// internally consistent.
func (i Image) Valid() bool {
	if i.name == "" || i.width <= 0 || i.height <= 0 || len(i.data) == 0 ||
		len(i.data) > MaxImageBytes ||
		(i.mimeType != "image/png" && i.mimeType != normalizedJPEGMediaType) {
		return false
	}

	return i.digest != ([sha256.Size]byte{})
}

// Equal reports content and metadata equality.
func (i Image) Equal(other Image) bool {
	return i.mimeType == other.mimeType && i.name == other.name &&
		i.width == other.width && i.height == other.height && i.digest == other.digest &&
		bytes.Equal(i.data, other.data)
}

// Name returns the bounded display name supplied at ingress.
func (i Image) Name() string { return i.name }

// MIMEType returns the normalized IANA media type.
func (i Image) MIMEType() string { return i.mimeType }

// Width returns the normalized pixel width.
func (i Image) Width() int { return i.width }

// Height returns the normalized pixel height.
func (i Image) Height() int { return i.height }

// Size returns the normalized encoded byte count.
func (i Image) Size() int { return len(i.data) }

// Digest returns the SHA-256 digest of normalized bytes.
func (i Image) Digest() [sha256.Size]byte { return i.digest }

// Bytes returns detached normalized bytes.
func (i Image) Bytes() []byte { return slices.Clone(i.data) }

// Part returns a provider-neutral inline image part with detached bytes.
func (i Image) Part() ai.ImagePart {
	return ai.ImageData(i.mimeType, slices.Clone(i.data))
}

func newImage(name, mimeType string, width, height int, data []byte) Image {
	detached := slices.Clone(data)

	return Image{
		data:     detached,
		mimeType: mimeType,
		name:     name,
		width:    width,
		height:   height,
		digest:   sha256.Sum256(detached),
	}
}

func validateImageName(name string) error {
	if name == "" || len(name) > MaxPathBytes || !utf8.ValidString(name) {
		return fmt.Errorf("%w: invalid image name", ErrInvalidImage)
	}

	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: invalid image name", ErrInvalidImage)
		}
	}

	return nil
}

func validateImageDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%w: invalid dimensions", ErrInvalidImage)
	}

	pixelsWide := uint64(width)

	pixelsHigh := uint64(height)
	if pixelsWide > uint64(MaxImagePixels)/pixelsHigh {
		return fmt.Errorf("%w: image exceeds pixel limit", ErrLimit)
	}

	return nil
}

func decodeImageFirstFrame(format string, encoded []byte) (image.Image, error) {
	reader := bytes.NewReader(encoded)

	switch format {
	case imageFormatPNG:
		return png.Decode(reader)
	case imageFormatJPEG:
		return jpeg.Decode(reader)
	case imageFormatGIF:
		// gif.Decode intentionally decodes only the first frame.
		return gif.Decode(reader)
	default:
		return nil, ErrUnsupportedImage
	}
}

func fittedDimensions(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}

	if width >= height {
		return maxEdge, max(1, int(int64(height)*int64(maxEdge)/int64(width)))
	}

	return max(1, int(int64(width)*int64(maxEdge)/int64(height))), maxEdge
}

func resizeOnWhite(
	ctx context.Context,
	source image.Image,
	width int,
	height int,
) (*image.RGBA, error) {
	bounds := source.Bounds()

	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		sourceY := bounds.Min.Y + int(int64(y)*int64(bounds.Dy())/int64(height))
		for x := range width {
			sourceX := bounds.Min.X + int(int64(x)*int64(bounds.Dx())/int64(width))
			red, green, blue, alpha := source.At(sourceX, sourceY).RGBA()
			result.SetRGBA(x, y, color.RGBA{
				R: compositeOnWhite(red, alpha),
				G: compositeOnWhite(green, alpha),
				B: compositeOnWhite(blue, alpha),
				A: 0xff,
			})
		}
	}

	return result, nil
}

func compositeOnWhite(channel, alpha uint32) uint8 {
	value := (channel + 0xffff - alpha) / 0x101

	return uint8(value) //nolint:gosec // RGBA guarantees channel <= alpha, so value is 0..255.
}

func unsupportedImageSignature(encoded []byte) bool {
	if len(encoded) >= 12 && string(encoded[:4]) == "RIFF" && string(encoded[8:12]) == "WEBP" {
		return true
	}

	if len(encoded) < 12 || string(encoded[4:8]) != "ftyp" {
		return false
	}

	brand := string(encoded[8:12])

	return strings.HasPrefix(brand, "hei") || strings.HasPrefix(brand, "hev") ||
		brand == "mif1" || brand == "msf1"
}
