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

func (e EchonetliteFrame) Encode() []byte {
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

func (e EchonetliteEdata) Encode() []byte {
	var b []byte
	b = append(b, e.epc, e.pdc)
	b = append(b, e.edt...)
	return b
}

func ParseEchonetliteFrame(data []byte) (EchonetliteFrame, error) {
	if len := len(data); len <= 12 {
		return EchonetliteFrame{}, fmt.Errorf("bad length(%d)", len)
	}
	//
	ehd := binary.BigEndian.Uint16(data[0:2])
	if ehd != 0x1081 {
		return EchonetliteFrame{}, fmt.Errorf("ehd:%x this is not an echonetlite frame", ehd)
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
	return EchonetliteFrame{
		ehd:   ehd,
		tid:   tid,
		seoj:  [3]byte(seoj),
		deoj:  [3]byte(deoj),
		esv:   esv,
		opc:   opc,
		edata: edata,
	}, nil
}

func ShowEchonetliteFrame(frame EchonetliteFrame) {
	switch frame.esv {
	case 0x72: // Get_res
		for _, v := range frame.edata {
			ShowEdataGetResponse(v)
		}
	case 0x73: // INF
		fallthrough
	default:
		slog.Debug("Echonetlite", slog.Any("frame", frame))
	}
}

func ShowEdataGetResponse(edata EchonetliteEdata) {
	switch edata.epc {
	case 0x80: // 動作状態
		switch edata.edt[0] {
		case 0x30:
			slog.Info("動作中")
		case 0x31:
			slog.Info("未動作")
		default:
			slog.Info("N/A")
		}
	case 0x81: // 設置場所
	case 0x88: // 異常発生状態
		switch edata.edt[0] {
		case 0x41:
			slog.Info("異常発生あり")
		case 0x42:
			slog.Info("異常発生なし")
		default:
			slog.Info("N/A")
		}
	case 0x8a: // メーカーコード
		manufacturer := [3]byte{}
		copy(manufacturer[:], edata.edt)
		slog.Info("製造者", slog.String("code(hex)", hex.EncodeToString(manufacturer[:])))
	case 0xd3: // 係数
		slog.Info("係数", slog.Int("multiplier", int(edata.edt[0])))
	case 0xd7: // 積算電力量有効桁数
		slog.Info("積算電力量有効桁数", slog.Int("digits", int(edata.edt[0])))
	case 0xe0: // 積算電力量計測値(正方向計測値)
		cwh := binary.BigEndian.Uint32(edata.edt)
		slog.Info("積算電力量", slog.Uint64("Wh", uint64(cwh)))
	case 0xe1: // 積算電力量単位(正方向、逆方向計測値)
		var powersOfTen int
		switch edata.edt[0] {
		case 0x00:
			powersOfTen = 0
		case 0x01:
			powersOfTen = -1
		case 0x02:
			powersOfTen = -2
		case 0x03:
			powersOfTen = -3
		case 0x04:
			powersOfTen = -4
		case 0x0a:
			powersOfTen = 1
		case 0x0b:
			powersOfTen = 2
		case 0x0c:
			powersOfTen = 3
		case 0x0d:
			powersOfTen = 4
		default:
			slog.Info("N/A")
		}
		s := fmt.Sprintf("%f kWh", math.Pow10(powersOfTen))
		slog.Info("積算電力量単位", slog.String("unit", s))
	case 0xe7: // 瞬時電力計測値
		iwatt := int32(binary.BigEndian.Uint32(edata.edt))
		slog.Info("瞬時電力", slog.Int("W", int(iwatt)))
	case 0xe8: // 瞬時電流計測値
		r := binary.BigEndian.Uint16(edata.edt[0:2])
		t := binary.BigEndian.Uint16(edata.edt[2:4])
		iampereR := float32(int16(r)) / 10.0
		if t == 0x7ffe {
			//単相2線式
			slog.Info("瞬時電流(単相2線式)", slog.Float64("R", float64(iampereR)))
		} else {
			iampereT := float32(t) / 10.0
			slog.Info(
				"瞬時電流(単相3線式)",
				slog.Float64("R", float64(iampereR)),
				slog.Float64("T", float64(iampereT)),
			)
		}
	default:
		slog.Debug("Echonetlite",
			slog.String("epc(hex)", strconv.FormatInt(int64(edata.epc), 16)),
			slog.String("pdc(hex)", strconv.FormatInt(int64(edata.pdc), 16)),
			slog.String("edt(hex)", hex.EncodeToString(edata.edt)),
		)
	}
}
