// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cmder

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// buildSyntheticQr 生成一张 n x n 模块、每模块 scale 像素的合成二维码黑白位图。
// 只描绘定位图形/分隔符/定时图形等结构模块, 其余数据区域用棋盘格填充,
// 用于验证 detectQrGrid 的拟合识别逻辑。
func buildSyntheticQr(n, scale int) [][]bool {
	size := n * scale
	bin := make([][]bool, size)
	for y := range bin {
		bin[y] = make([]bool, size)
	}
	paintModule := func(r, c int, dark bool) {
		for dy := 0; dy < scale; dy++ {
			for dx := 0; dx < scale; dx++ {
				bin[r*scale+dy][c*scale+dx] = dark
			}
		}
	}
	// 数据区域铺棋盘格
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			paintModule(r, c, (r+c)%2 == 0)
		}
	}
	// 三个角的定位图形 + 分隔符(定位图形外一圈 1 模块宽的白边, 越出二维码边界的部分省略)
	paintFinder := func(r0, c0 int) {
		for r := 0; r < 7; r++ {
			for c := 0; c < 7; c++ {
				paintModule(r0+r, c0+c, finder7x7[r][c])
			}
		}
		for rr := -1; rr <= 7; rr++ {
			for cc := -1; cc <= 7; cc++ {
				if rr == -1 || rr == 7 || cc == -1 || cc == 7 {
					if r0+rr >= 0 && r0+rr < n && c0+cc >= 0 && c0+cc < n {
						paintModule(r0+rr, c0+cc, false)
					}
				}
			}
		}
	}
	paintFinder(0, 0)
	paintFinder(0, n-7)
	paintFinder(n-7, 0)
	// 第 6 行定时图形: 黑白交替
	for c := 8; c <= n-9; c++ {
		paintModule(6, c, (c-8)%2 == 0)
	}
	return bin
}

// syntheticQrToPNG 把合成位图编码成 PNG 字节
func syntheticQrToPNG(bin [][]bool) []byte {
	h, w := len(bin), len(bin[0])
	gray := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(255)
			if bin[y][x] {
				v = 0
			}
			gray.SetGray(x, y, color.Gray{Y: v})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, gray)
	return buf.Bytes()
}

func TestDetectQrGrid(t *testing.T) {
	bin := buildSyntheticQr(29, 4)
	n, ox, oy, m, ok := detectQrGrid(bin)
	if !ok {
		t.Fatal("detectQrGrid 未能识别出合成二维码")
	}
	if n != 29 {
		t.Fatalf("识别到的模块边长 = %d, 期望 29", n)
	}
	if m != 4 {
		t.Fatalf("识别到的模块像素大小 = %v, 期望 4", m)
	}
	if ox != 0 || oy != 0 {
		t.Fatalf("识别到的模块(0,0)坐标 = (%v,%v), 期望 (0,0)", ox, oy)
	}
}

func TestQrArtFromImageBytes(t *testing.T) {
	bin := buildSyntheticQr(29, 4)
	art, err := qrArtFromImageBytes(syntheticQrToPNG(bin))
	if err != nil {
		t.Fatalf("qrArtFromImageBytes 失败: %v", err)
	}
	lines := strings.Split(strings.TrimRight(art, "\n"), "\n")
	// 安静区 4 模块宽: 总列数 = 37; 每个字符覆盖 2 行模块, 故总行数 = ceil(37/2) = 19
	const cols = 37
	if len(lines) != 19 {
		t.Fatalf("字符画行数 = %d, 期望 19", len(lines))
	}
	for i, line := range lines {
		if len([]rune(line)) != cols {
			t.Fatalf("第 %d 行宽度 = %d, 期望 %d", i, len([]rune(line)), cols)
		}
	}
	// 安静区全为浅色, 应整行都是前景填充字符
	if strings.Trim(lines[0], "█") != "" {
		t.Fatalf("安静区首行应为全浅色:'████...', 实际: %q", lines[0])
	}
	// 定位图形区域应包含深色模块留白(空格)
	if !strings.Contains(art, " ") {
		t.Fatal("二维码中应包含深色模块(空格)部分")
	}
}
