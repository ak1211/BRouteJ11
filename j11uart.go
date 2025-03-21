// BP35Cx-J11を使ってスマートメータから電力消費量などを得る
// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2025 Akihiro Yamamoto <github.com/ak1211>
package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/netip"
)

// チェックサム計算
func CalcChecksum(data []byte) uint16 {
	var acc uint16
	for _, v := range data {
		acc += uint16(v)
		acc &= 0xffff
	}
	return acc
}

const J11DatagramHeaderBytes int = 12

type J11DatagramHeader struct {
	UniqueCode     uint32
	CommandCode    uint16
	MessageLen     uint16
	HeaderChecksum uint16
	DataChecksum   uint16
}

// ヘッダ部チェックサム計算
func (h J11DatagramHeader) CalcHeaderChecksum() uint16 {
	buf := binary.BigEndian.AppendUint32([]byte{}, h.UniqueCode)
	buf = binary.BigEndian.AppendUint16(buf, h.CommandCode)
	buf = binary.BigEndian.AppendUint16(buf, h.MessageLen)
	return CalcChecksum(buf)
}

type J11Datagram struct {
	Header J11DatagramHeader
	Data   []byte
}

func (c J11Datagram) Write(w io.Writer) (int, error) {
	buf := make([]byte, J11DatagramHeaderBytes)
	n, err := binary.Encode(buf, binary.BigEndian, c.Header)
	if err != nil {
		slog.Error("Encode", "err", err)
		return 0, err
	}
	buf = append(buf[0:n], c.Data...)
	n, err = w.Write(buf)
	if err != nil {
		slog.Error("Write", "err", err)
		return n, err
	}
	return n, nil
}

// アクティブスキャンに応答したスマートメーターの情報
type BeaconResponse struct {
	channel    uint8
	macAddress uint64
	panId      uint16
	rssi       int8
}

type RouteBId [32]byte
type RouteBPassword [12]byte

// ユニークコード(要求コマンド)
const UniqueCodeRequestCommand uint32 = 0xd0ea83fc

// ユニークコード(応答/通知コマンド)
const UniqueCodeResponseCommand uint32 = 0xd0f9ee5d

// ファームウェアバージョン取得コマンド
func CommandGetFirmwareVersion() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x006b,
			MessageLen:     0x0004,
			HeaderChecksum: 0x03a8,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// ハードウェアリセットコマンド
func CommandHardwareReset() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x00d9,
			MessageLen:     0x0004,
			HeaderChecksum: 0x0000,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// 初期設定要求コマンド
func CommandInitialSetup(channel uint8) J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x005f,
			MessageLen:     0x0008,
			HeaderChecksum: 0x0000,
			DataChecksum:   0x0000,
		},
		Data: []byte{0x05, 0x00, channel, 0x00},
	}
}

// PANA認証情報設定コマンド
func CommandSetPanaAuthInfo(routeBId RouteBId, routeBPassword RouteBPassword) J11Datagram {
	data := routeBId[:]                       // 認証ID(32バイト)
	data = append(data, routeBPassword[:]...) // 認証パスワード(12バイト)
	command := J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0054,
			MessageLen:     0x0030,
			HeaderChecksum: 0x03bd,
			DataChecksum:   0,
		},
		Data: data,
	}
	command.Header.DataChecksum = CalcChecksum(command.Data)
	return command
}

// Bルート動作開始要求コマンド
func CommandBRouteStart() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0053,
			MessageLen:     0x0004,
			HeaderChecksum: 0x0390,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// Bルート動作終了要求コマンド
func CommandBRouteTerminate() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0053,
			MessageLen:     0x0004,
			HeaderChecksum: 0x0395,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// アクティブスキャン実行要求コマンド
func CommandActivescan(scanDuration uint8, routeBId RouteBId) J11Datagram {
	data := []byte{scanDuration}                           // スキャン時間(1バイト)
	data = append(data, []byte{0x00, 0x03, 0xff, 0xf0}...) // スキャンチャネル4,5,6指定(4バイト)
	data = append(data, 0x01)                              // ID設定(1バイト)
	data = append(data, routeBId[len(routeBId)-8:]...)     // Ｂルート認証IDの最後8文字(8バイト)
	command := J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0051,
			MessageLen:     0x0012,
			HeaderChecksum: 0x039c,
			DataChecksum:   0,
		},
		Data: data,
	}
	command.Header.DataChecksum = CalcChecksum(command.Data)
	return command
}

// UDPポートオープン要求コマンド
func CommandUdpPortOpen(port uint16) J11Datagram {
	data := make([]byte, 2)
	binary.BigEndian.PutUint16(data, port)
	command := J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0005,
			MessageLen:     0x0006,
			HeaderChecksum: 0x0344,
			DataChecksum:   0,
		},
		Data: data,
	}
	command.Header.DataChecksum = CalcChecksum(command.Data)
	return command
}

// BルートPANA開始要求コマンド
func CommandBRouteStartPana() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0056,
			MessageLen:     0x0004,
			HeaderChecksum: 0x0393,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// BルートPANA終了要求コマンド
func CommandBRouteTerminatePana() J11Datagram {
	return J11Datagram{
		Header: J11DatagramHeader{
			UniqueCode:     UniqueCodeRequestCommand,
			CommandCode:    0x0057,
			MessageLen:     0x0004,
			HeaderChecksum: 0x0394,
			DataChecksum:   0x0000,
		},
		Data: []byte{},
	}
}

// データ送信要求コマンド
func CommandTransmitData(ipv6 netip.Addr, payload []byte) (J11Datagram, error) {
	data := ipv6.AsSlice() // 送信元IPv6アドレス(16バイト)
	if len(data) == 16 {
		data = binary.BigEndian.AppendUint16(data, 0x0e1a)               // 送信元ポート番号(2バイト)
		data = binary.BigEndian.AppendUint16(data, 0x0e1a)               // 送信先ポート番号(2バイト)
		data = binary.BigEndian.AppendUint16(data, uint16(len(payload))) // 送信データ長(2バイト)
		data = append(data, payload...)                                  // 送信データ(任意バイト)
		command := J11Datagram{
			Header: J11DatagramHeader{
				UniqueCode:     UniqueCodeRequestCommand,
				CommandCode:    0x0008,
				MessageLen:     4 + uint16(len(data)),
				HeaderChecksum: 0,
				DataChecksum:   0,
			},
			Data: data,
		}
		command.Header.HeaderChecksum = command.Header.CalcHeaderChecksum()
		command.Header.DataChecksum = CalcChecksum(command.Data)
		return command, nil
	} else {
		return J11Datagram{}, errors.New("bad ipv6 address")
	}
}
