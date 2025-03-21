// BP35Cx-J11を使ってスマートメータから電力消費量などを得る
// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2025 Akihiro Yamamoto <github.com/ak1211>
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
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

type EchonetliteEdata struct {
	epc byte
	pdc byte
	edt []byte
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
	case 0xe0: // 積算電力量計測値
		cwh := binary.BigEndian.Uint32(edata.edt)
		slog.Info("cumlative watt hour", slog.Uint64("Wh", uint64(cwh)))
	case 0xe7: // 瞬時電力計測値
		iwatt := int32(binary.BigEndian.Uint32(edata.edt))
		slog.Info("instantious watt", slog.Int("W", int(iwatt)))
	case 0xe8: // 瞬時電流計測値
		r := binary.BigEndian.Uint16(edata.edt[0:2])
		t := binary.BigEndian.Uint16(edata.edt[2:4])
		iampereR := float32(int16(r)) / 10.0
		if t == 0x7ffe {
			//単相2線式
			slog.Info("instantious ampere", slog.Float64("R", float64(iampereR)))
		} else {
			iampereT := float32(t) / 10.0
			slog.Info(
				"instantious ampere",
				slog.Float64("R", float64(iampereR)),
				slog.Float64("T", float64(iampereT)),
			)
		}
	default:
		slog.Debug("Echonetlite",
			slog.String("epc", strconv.FormatInt(int64(edata.epc), 16)),
			slog.String("pdc", strconv.FormatInt(int64(edata.pdc), 16)),
			slog.String("edt", hex.EncodeToString(edata.edt)),
		)
	}
}
