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
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"strings"
)

// renderQrLoginImage 把二维码图片解析后直接在终端打印, 无需再打开图片文件。
// 用 Unicode 半块字符(▀/▄/█)绘制: 1 列字符 = 1 个模块列、1 行字符 = 2 行模块,
// 与终端字符约 1:2 的宽高比匹配, 画出的是正方形二维码, 手机可直接扫码。
// 终端黑底时, 深色模块直接露出背景色(黑)、浅色模块用前景色(白)填充,
// 得到白底黑方块的标准二维码外观。
func renderQrLoginImage(imgPath string) error {
	art, err := qrArtFromImageFile(imgPath)
	if err != nil {
		return err
	}
	fmt.Print(art)
	return nil
}

// qrArtFromImageFile 读取图片文件并渲染成终端字符画
func qrArtFromImageFile(imgPath string) (string, error) {
	data, err := os.ReadFile(imgPath)
	if err != nil {
		return "", err
	}
	return qrArtFromImageBytes(data)
}

// decodeImage 解码 PNG/JPEG 图片字节
func decodeImage(data []byte) (image.Image, string, error) {
	return image.Decode(bytes.NewReader(data))
}

// qrArtFromImageBytes 解析图片字节并渲染成终端字符画
func qrArtFromImageBytes(data []byte) (string, error) {
	img, _, err := decodeImage(data)
	if err != nil {
		return "", err
	}
	bin := binarizeImage(img)
	n, ox, oy, m, ok := detectQrGrid(bin)
	if !ok {
		return "", errors.New("未能识别出二维码模块")
	}
	return qrTerminalArt(bin, n, ox, oy, m), nil
}

// binarizeImage 把图片转成黑白(boolean)像素矩阵, true=深色
func binarizeImage(img image.Image) [][]bool {
	rect := img.Bounds()
	w, h := rect.Dx(), rect.Dy()
	bin := make([][]bool, h)
	for y := 0; y < h; y++ {
		row := make([]bool, w)
		for x := 0; x < w; x++ {
			gray := color.GrayModel.Convert(img.At(rect.Min.X+x, rect.Min.Y+y)).(color.Gray)
			row[x] = gray.Y < 128
		}
		bin[y] = row
	}
	return bin
}

// blackBbox 深色像素的包围盒, 即二维码有效区域(不含四周的空白安静区)
func blackBbox(bin [][]bool) (x0, y0, x1, y1 int, ok bool) {
	x0, y0 = len(bin[0]), len(bin)
	x1, y1 = -1, -1
	for y := 0; y < len(bin); y++ {
		row := bin[y]
		for x := 0; x < len(row); x++ {
			if row[x] {
				if x < x0 {
					x0 = x
				}
				if x > x1 {
					x1 = x
				}
				if y < y0 {
					y0 = y
				}
				if y > y1 {
					y1 = y
				}
			}
		}
	}
	ok = x0 <= x1 && y0 <= y1
	return
}

// detectQrGrid 识别二维码的模块边长 n, 以及模块(0,0)的像素坐标 (ox,oy) 和每个模块的像素大小 m。
// 逐版本拟合: 标准 QR 模块边长 n = 17 + 4*版本号(版本号 1~40),
// 用三个角的定位图形(7x7 固定图案)与第 6 行定时图形(黑白交替)双重校验。
func detectQrGrid(bin [][]bool) (n int, ox, oy, m float64, ok bool) {
	x0, y0, x1, y1, found := blackBbox(bin)
	if !found {
		return
	}
	bw := float64(x1 - x0 + 1)
	bh := float64(y1 - y0 + 1)
	ox, oy = float64(x0), float64(y0)
	for version := 1; version <= 40; version++ {
		n = 17 + 4*version
		m = bw / float64(n)
		mh := bh / float64(n)
		// 模块像素尺寸过小或包围盒长宽不一致, 跳过
		if m < 3 || math.Abs(m/mh-1) > 0.15 {
			continue
		}
		// 三个角的定位图形都要命中
		if !finderOK(bin, ox, oy, m, 0, 0) {
			continue
		}
		if !finderOK(bin, ox, oy, m, 0, n-7) {
			continue
		}
		if !finderOK(bin, ox, oy, m, n-7, 0) {
			continue
		}
		// 定时图形黑白交替校验, 排除误判
		if !timingOK(bin, ox, oy, m, n) {
			continue
		}
		return n, ox, oy, m, true
	}
	return 0, 0, 0, 0, false
}

// finder7x7 二维码定位图形的 7x7 模块模版, true=深色
var finder7x7 = [7][7]bool{
	{true, true, true, true, true, true, true},
	{true, false, false, false, false, false, true},
	{true, false, true, true, true, false, true},
	{true, false, true, true, true, false, true},
	{true, false, true, true, true, false, true},
	{true, false, false, false, false, false, true},
	{true, true, true, true, true, true, true},
}

// finderOK 检查以 (r0,c0) 为左上角的 7x7 模块区域是否为定位图形
func finderOK(bin [][]bool, ox, oy, m float64, r0, c0 int) bool {
	for r := 0; r < 7; r++ {
		for c := 0; c < 7; c++ {
			if sampleModule(bin, ox, oy, m, r0+r, c0+c) != finder7x7[r][c] {
				return false
			}
		}
	}
	return true
}

// timingOK 校验第 6 行定时图形(两个定位图形之间的一段模块)是否黑白交替
func timingOK(bin [][]bool, ox, oy, m float64, n int) bool {
	prev := sampleModule(bin, ox, oy, m, 6, 8)
	for c := 9; c <= n-9; c++ {
		cur := sampleModule(bin, ox, oy, m, 6, c)
		if cur == prev {
			return false
		}
		prev = cur
	}
	return true
}

// sampleModule 取第 r 行第 c 列模块中心像素的颜色, 越界视为浅色
func sampleModule(bin [][]bool, ox, oy, m float64, r, c int) bool {
	x := int(math.Round(ox + (float64(c)+0.5)*m))
	y := int(math.Round(oy + (float64(r)+0.5)*m))
	if y < 0 || y >= len(bin) || x < 0 || x >= len(bin[y]) {
		return false
	}
	return bin[y][x]
}

// moduleDark 取模块 (r,c) 是否深色, 越界(安静区)视为浅色
func moduleDark(bin [][]bool, ox, oy, m float64, n, r, c int) bool {
	if r < 0 || r >= n || c < 0 || c >= n {
		return false
	}
	return sampleModule(bin, ox, oy, m, r, c)
}

// qrTerminalArt 把识别出的二维码渲染成终端字符画, 每个字符覆盖 2 行 x 1 列模块。
// 四周补 4 个模块宽的安静区, 便于手机扫码。
func qrTerminalArt(bin [][]bool, n int, ox, oy, m float64) string {
	const quietZone = 4
	total := n + quietZone*2
	var b strings.Builder
	b.Grow((total + 1) * (total + 1))
	for y := 0; y < total; y += 2 {
		for x := 0; x < total; x++ {
			top := moduleDark(bin, ox, oy, m, n, y-quietZone, x-quietZone)
			bottom := moduleDark(bin, ox, oy, m, n, y-quietZone+1, x-quietZone)
			b.WriteRune(qrCellChar(top, bottom))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// qrCellChar 把上下相邻的两个模块映射成半块字符。
// 终端黑底时: 浅色模块用前景(白)填充、深色模块露出背景(黑),
// 得到白底黑方块的标准二维码外观。
func qrCellChar(topDark, bottomDark bool) rune {
	switch {
	case topDark && bottomDark:
		return ' ' // 上深下深: 整格露出背景
	case !topDark && !bottomDark:
		return '█' // 上浅下浅: 整格前景填充
	case topDark && !bottomDark:
		return '▄' // 上深(背景) 下浅(前景)
	default:
		return '▀' // 上浅(前景) 下深(背景)
	}
}
