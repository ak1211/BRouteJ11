// BP35Cx-J11を使ってスマートメータから電力消費量などを得る
// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2025 Akihiro Yamamoto <github.com/ak1211>
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

type EchonetliteFrame struct {
	ehd   uint16
	tid   uint16
	seoj  [3]byte
	deoj  [3]byte
	esv   byte
	opc   byte
	edata []EchonetliteEdata
}

func (e *EchonetliteFrame) Encode() []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, e.ehd)
	b = binary.BigEndian.AppendUint16(b, e.tid)
	b = append(b, e.seoj[:]...)
	b = append(b, e.deoj[:]...)
	b = append(b, e.esv, e.opc)
	for _, v := range e.edata {
		b = append(b, v.Encode()...)
	}
	return b
}

type EchonetliteEdata struct {
	epc byte
	pdc byte
	edt []byte
}

func (e *EchonetliteEdata) Encode() []byte {
	var b []byte
	b = append(b, e.epc, e.pdc)
	b = append(b, e.edt...)
	return b
}

func ParseEchonetliteFrame(data []byte) (*EchonetliteFrame, error) {
	if len := len(data); len <= 12 {
		return nil, fmt.Errorf("bad length(%d)", len)
	}
	//
	ehd := binary.BigEndian.Uint16(data[0:2])
	if ehd != 0x1081 {
		return nil, fmt.Errorf("ehd:%x this is not an echonetlite frame", ehd)
	}
	tid := binary.BigEndian.Uint16(data[2:4])
	seoj := data[4:7]
	deoj := data[7:10]
	esv := data[10]
	opc := data[11]
	props := data[12:]
	var edata []EchonetliteEdata
	for count := 0; count < int(opc); count++ {
		edata = append(edata, EchonetliteEdata{
			epc: props[0],              // 要求
			pdc: props[1],              // データ数
			edt: props[2 : 2+props[1]], // データ
		})
		props = props[2+props[1]:]
	}
	//
	return &EchonetliteFrame{
		ehd:   ehd,
		tid:   tid,
		seoj:  [3]byte(seoj),
		deoj:  [3]byte(deoj),
		esv:   esv,
		opc:   opc,
		edata: edata,
	}, nil
}

func (e *EchonetliteFrame) Show() {
	n := len(e.edata)
	switch e.esv {
	case 0x52: // Get_SNA
		slog.Info("プロパティ値読み出し不可応答", slog.Int("N", n))
	case 0x53: // INF_SNA
		slog.Info("プロパティ値通知不可応答", slog.Int("N", n))
	case 0x71: // Set_res
		slog.Info("プロパティ値書き込み応答", slog.Int("N", n))
	case 0x72: // Get_res
		slog.Info("プロパティ値読み出し応答", slog.Int("N", n))
	case 0x73: // INF
		slog.Info("プロパティ値通知", slog.Int("N", n))
	default:
		slog.Debug("よくわからないESV値", slog.Any("frame", e))
	}
	for i := 0; i < n; i++ {
		e.edata[i].Show()
	}
}

// EDATA値を表示する
func (e *EchonetliteEdata) Show() {
	switch e.epc {
	case 0x80: // 動作状態
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		switch {
		case e.edt[0] == 0x30:
			s = "動作中"
		case e.edt[0] == 0x31:
			s = "未動作"
		}
		slog.Info("edata", slog.String("動作状態", s))
	case 0x88: // 異常発生状態
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		switch {
		case e.edt[0] == 0x41:
			s = "異常発生あり"
		case e.edt[0] == 0x42:
			s = "異常発生なし"
		}
		slog.Info("edata", slog.String("異常発生状態", s))
	case 0x8a: // メーカーコード
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 3 {
			manufacturer := [3]byte{}
			copy(manufacturer[:], e.edt)
			s = hex.EncodeToString(manufacturer[:])
		}
		slog.Info("edata", slog.String("製造者コード(hex)", s))
	case 0xd3: // 係数
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 1 {
			s = strconv.FormatInt(int64(e.edt[0]), 10)
		}
		slog.Info("edata", slog.String("係数", s))
	case 0xd7: // 積算電力量有効桁数
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 1 {
			s = strconv.FormatInt(int64(e.edt[0]), 10)
		}
		slog.Info("edata", slog.String("積算電力量有効桁数", s+" 桁"))
	case 0xe0: // 積算電力量計測値(正方向計測値)
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 4 {
			cwh := binary.BigEndian.Uint32(e.edt)
			s = strconv.FormatInt(int64(cwh), 10)
		}
		slog.Info("edata", slog.String("積算電力量", s))
	case 0xe1: // 積算電力量単位(正方向、逆方向計測値)
		var powersOfTen int
		switch {
		case e.edt[0] == 0x00:
			powersOfTen = 0
		case e.edt[0] == 0x01:
			powersOfTen = -1
		case e.edt[0] == 0x02:
			powersOfTen = -2
		case e.edt[0] == 0x03:
			powersOfTen = -3
		case e.edt[0] == 0x04:
			powersOfTen = -4
		case e.edt[0] == 0x0a:
			powersOfTen = 1
		case e.edt[0] == 0x0b:
			powersOfTen = 2
		case e.edt[0] == 0x0c:
			powersOfTen = 3
		case e.edt[0] == 0x0d:
			powersOfTen = 4
		default:
			powersOfTen = 0xff
		}
		s := fmt.Sprintf("%f kWh", math.Pow10(powersOfTen))
		slog.Info("edata", slog.String("積算電力量単位", s))
	case 0xe2: // 積算電力量計測値履歴1 (正方向計測値)
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 194 {
			day := binary.BigEndian.Uint16(e.edt[0:2])
			var ss [48]string
			for i := 0; i < 48; i++ {
				v := binary.BigEndian.Uint32(e.edt[2+4*i:])
				if v == 0xfffffffe {
					ss[i] = fmt.Sprintf("%8s", "N/A")
				} else {
					ss[i] = fmt.Sprintf("%8d", v)
				}
			}
			s = fmt.Sprintf("%d日前[", day) + strings.Join(ss[:], ",") + "]"
		}
		slog.Info("edata", slog.String("積算電力量計測値履歴1 (正方向計測値)", s))
	case 0xe7: // 瞬時電力計測値
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 4 {
			iwatt := binary.BigEndian.Uint32(e.edt)
			s = strconv.FormatInt(int64(iwatt), 10)
		}
		slog.Info("edata", slog.String("瞬時電力", s+" W"))
	case 0xe8: // 瞬時電流計測値
		s := fmt.Sprintf("N/A(epc:0x%02x)", e.epc)
		if len(e.edt) >= 4 {
			r := binary.BigEndian.Uint16(e.edt[0:2])
			t := binary.BigEndian.Uint16(e.edt[2:4])
			if t == 0x7ffe { // 単相2線式
				s = fmt.Sprintf("(1φ2W) %3d.%01d", r/10, r%10)
			} else {
				s = fmt.Sprintf("(1φ3W) R:%3d.%01d, T:%3d.%01d", r/10, r%10, t/10, t%10)
			}
		}
		slog.Info("edata", slog.String("瞬時電流", s))
	case 0xea: // 定時積算電力量計測値(正方向計測値)
		s := "N/A"
		if len(e.edt) >= 11 {
			year := binary.BigEndian.Uint16(e.edt[0:2])
			month := e.edt[2]
			day := e.edt[3]
			hour := e.edt[4]
			minute := e.edt[5]
			second := e.edt[6]
			cwh := binary.BigEndian.Uint32(e.edt[7:])
			s = fmt.Sprintf("%04d/%02d/%02d %02d:%02d:%02d (%8d)", year, month, day, hour, minute, second, cwh)
		}
		slog.Info("edata", slog.String("定時積算電力量計測値(正方向計測値)", s))
	default:
		slog.Debug("edata",
			slog.String("epc(hex)", strconv.FormatInt(int64(e.epc), 16)),
			slog.String("pdc(hex)", strconv.FormatInt(int64(e.pdc), 16)),
			slog.String("edt(hex)", hex.EncodeToString(e.edt)),
		)
	}
}
