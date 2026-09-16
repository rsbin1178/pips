// Command agnes-image generates and edits images with the Agnes Image API.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/agnes"
)

func main() {
	if err := run(); err != nil {
		fail(err)
	}
}

func run() error {
	// 单次生成可能耗时数十秒，Agnes 官方建议客户端超时 60–360s。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 默认 base URL 是中国站，国际站用 agnes.WithBaseURL(agnes.BaseURLGlobal)。
	// 密钥默认读 AGNES_API_KEY，也可 agnes.WithAPIKey("sk-...") 显式传入。
	model := agnes.NewImageModel("agnes-image-2.5-flash", agnes.WithAPIKey(os.Getenv("API_KEY")))

	if err := generateCity(ctx, model); err != nil {
		return fmt.Errorf("文生图: %w", err)
	}

	if err := generateInlineCube(ctx, model); err != nil {
		return fmt.Errorf("文生图（Base64）: %w", err)
	}

	if err := editCityToNight(ctx, model); err != nil {
		return fmt.Errorf("图生图: %w", err)
	}

	if err := composePoster(ctx, model); err != nil {
		return fmt.Errorf("多图合成: %w", err)
	}

	return nil
}

// 1) 文生图：size 是必填档位，与 ratio 组合决定实际像素。
func generateCity(ctx context.Context, model *agnes.ImageModel) error {
	gen, err := model.GenerateImages(ctx, ai.ImageRequest{
		Prompt: "日出时分薄雾峡谷上方的发光浮空城市，电影级写实风格，广角构图，高视觉密度",
		Size:   "2K", // 2K + 16:9 = 2624x1472
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{
				Ratio:          "16:9", // 1:1（默认）、3:4、4:3、16:9、9:16、2:3、3:2、21:9
				ResponseFormat: "url",  // "url" 或 "b64_json"
			},
		},
	})
	if err != nil {
		return err
	}

	img, err := first(gen)
	if err != nil {
		return err
	}

	if err := save(ctx, img, "city.png"); err != nil {
		return fmt.Errorf("保存失败: %w", err)
	}

	fmt.Println("已保存 city.png，厂商改写后的提示词:", img.RevisedPrompt)

	return nil
}

// 2) 文生图直接拿字节：return_base64 只对文生图有效。
func generateInlineCube(ctx context.Context, model *agnes.ImageModel) error {
	inline, err := model.GenerateImages(ctx, ai.ImageRequest{
		Prompt: "白色背景上的玻璃方块产品图，柔和阴影，高细节",
		Size:   "1K",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{ReturnBase64: true},
		},
	})
	if err != nil {
		return err
	}

	img, err := first(inline)
	if err != nil {
		return err
	}

	if err := save(ctx, img, "cube.png"); err != nil {
		return fmt.Errorf("保存失败: %w", err)
	}

	fmt.Println("已保存 cube.png")

	return nil
}

// 3) 图生图：本地文件作为来源，适配器把它编码成 data URI 放进 extra_body.image。
func editCityToNight(ctx context.Context, model *agnes.ImageModel) error {
	source, err := os.ReadFile("city.png")
	if err != nil {
		return fmt.Errorf("读取来源图失败: %w", err)
	}

	edited, err := model.EditImage(ctx, ai.ImageEditRequest{
		Prompt: "改成雨夜霓虹风格，保留原始构图、相机角度和主要建筑形状",
		Images: []ai.ImagePart{ai.ImageData(http.DetectContentType(source), source)},
		Size:   "2K",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{Ratio: "16:9", ResponseFormat: "url"},
		},
	})
	if err != nil {
		return err
	}

	img, err := first(edited)
	if err != nil {
		return err
	}

	if err := save(ctx, img, "city-night.png"); err != nil {
		return fmt.Errorf("保存失败: %w", err)
	}

	fmt.Println("已保存 city-night.png")

	return nil
}

// 4) 多图合成：先生成两张参考图，再把这两张图作为输入合成海报。
func composePoster(ctx context.Context, model *agnes.ImageModel) error {
	character, err := generateReferenceImage(ctx, model, "角色设定图：银发机械师，正面站姿，简洁浅灰背景，全身，柔和棚拍光线", "ref-character.png")
	if err != nil {
		return fmt.Errorf("生成参考图（角色）: %w", err)
	}

	product, err := generateReferenceImage(ctx, model, "产品参考图：磨砂金属手环，纯色背景，柔和阴影，高细节", "ref-product.png")
	if err != nil {
		return fmt.Errorf("生成参考图（产品）: %w", err)
	}

	fmt.Println("已保存 ref-character.png 与 ref-product.png，开始多图合成")

	combined, err := model.EditImage(ctx, ai.ImageEditRequest{
		Prompt: "把第一张图作为主要角色、第二张图作为产品参考，生成电影级活动海报，保留角色身份与产品外形",
		Images: []ai.ImagePart{
			ai.ImageData(character.MIMEType, character.Data),
			ai.ImageData(product.MIMEType, product.Data),
		},
		Size: "2K",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{Ratio: "16:9", ResponseFormat: "url"},
		},
	})
	if err != nil {
		return err
	}

	img, err := first(combined)
	if err != nil {
		return err
	}

	if err := save(ctx, img, "poster.png"); err != nil {
		return fmt.Errorf("保存失败: %w", err)
	}

	fmt.Println("已保存 poster.png")

	return nil
}

func generateReferenceImage(ctx context.Context, model *agnes.ImageModel, prompt, filename string) (ai.GeneratedImage, error) {
	resp, err := model.GenerateImages(ctx, ai.ImageRequest{
		Prompt: prompt,
		Size:   "1K",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{ReturnBase64: true},
		},
	})
	if err != nil {
		return ai.GeneratedImage{}, err
	}

	img, err := first(resp)
	if err != nil {
		return ai.GeneratedImage{}, err
	}

	if err := save(ctx, img, filename); err != nil {
		return ai.GeneratedImage{}, fmt.Errorf("保存失败: %w", err)
	}

	return img, nil
}

// first returns the first image of a response.
func first(resp *ai.ImageResponse) (ai.GeneratedImage, error) {
	if resp == nil || len(resp.Images) == 0 {
		return ai.GeneratedImage{}, errors.New("响应里没有图片")
	}

	return resp.Images[0], nil
}

// save writes an image to disk. Inline bytes are written directly; a hosted URL
// is downloaded explicitly, because the adapter never fetches it for you.
func save(ctx context.Context, img ai.GeneratedImage, path string) error {
	if len(img.Data) > 0 {
		return os.WriteFile(path, img.Data, 0o600)
	}

	if img.URL == "" {
		return errors.New("图片既没有字节也没有 URL")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // example

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("下载图片失败: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

// fail logs a classified image failure and exits. Agnes failures carry the same
// portable error classes as every other adapter.
func fail(err error) {
	switch {
	case errors.Is(err, ai.ErrInvalidRequest):
		log.Fatalf("请求不合法，无需重试: %v", err)
	case errors.Is(err, ai.ErrUnsupported):
		log.Fatalf("Agnes 不支持该能力（多图生成 / mask / 文件 ID）: %v", err)
	case ai.IsRetryable(err):
		log.Fatalf("可重试错误（429/5xx/网络），Retry-After 已解析: %v", err)
	default:
		// 需要细节时用 errors.As 取结构化错误。
		var apiErr *ai.Error
		if errors.As(err, &apiErr) {
			log.Fatalf("HTTP %d type=%s code=%s message=%s", apiErr.StatusCode, apiErr.Type, apiErr.Code, apiErr.Message)
		}

		log.Fatalf("%v", err)
	}
}
