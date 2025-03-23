// BP35Cx-J11を使ってスマートメータから電力消費量などを得る
// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2025 Akihiro Yamamoto <github.com/ak1211>
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tarm/serial"
	"github.com/urfave/cli/v2"
)

// 設定
type Settings struct {
	RouteBId       string `json:"RouteBId"`
	RouteBPassword string `json:"RouteBPassword"`
	Channel        int    `json:"Channel"`
	MacAddress     string `json:"MacAddress"`
	PanId          int    `json:"PanId"`
}

// タイムアウト値
const UartReadTimeout time.Duration = 90 * time.Second

var ErrUartReadTimeoutExceeded = errors.New("UART read timeout exceeded")

// 積算電力量計測値を取得するechonet lite電文
func getElCumlativeWattHour() []byte {
	return []byte{
		0x10, 0x81, // 0x1081 = echonet lite
		0x00, 0x01, // tid
		0x05, 0xff, 0x01, // home controller
		0x02, 0x88, 0x01, // smartmeter
		0x62, // get要求
		0x01, // 1つ
		0xe0, // 積算電力量計測値(正方向計測値)
		0x00, // 送信するデータ無し
	}
}

// 瞬時電力と瞬時電流計測値を取得するechonet lite電文
func getElInstantWattAmpere() []byte {
	return []byte{
		0x10, 0x81, // 0x1081 = echonet lite
		0x00, 0x01, // tid
		0x05, 0xff, 0x01, // home controller
		0x02, 0x88, 0x01, // smartmeter
		0x62, // get要求
		0x02, // 2つ
		0xe7, // 瞬時電力計測値
		0x00, // 送信するデータ無し
		0xe8, // 瞬時電流計測値
		0x00, // 送信するデータ無し
	}
}

// スマートメーターを探す
func pairing(
	settingsFileName string,
	serialName string,
	scanDuration uint8,
	rbid RouteBId,
	rbpassword RouteBPassword,
) error {
	config := &serial.Config{
		Name:        serialName,
		Baud:        115200,
		ReadTimeout: 10 * time.Second,
		Size:        8,
	}
	stream, err := serial.OpenPort(config)
	if err != nil {
		return err
	}

	// コマンド応答チャネル
	rxDataChan := make(chan J11Datagram, 64)
	defer close(rxDataChan)
	// 通知チャネル
	rxNotifyChan := make(chan J11Datagram, 64)
	defer close(rxNotifyChan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uartReceiver(ctx, stream, rxDataChan, rxNotifyChan)

	//
	// ハードウェアリセット要求コマンドを発行する
	//
	_, err = CommandHardwareReset().Write(stream)
	if err != nil {
		return err
	}
	// 起動完了通知: 0x6019を確認するまで待つ
	for done := false; !done; {
		select {
		case r := <-rxNotifyChan:
			done = r.Header.CommandCode == 0x6019
		case <-time.After(UartReadTimeout):
			return errors.New("J11 UART hardware reset command has no response")
		}
	}

	//
	// 初期設定要求コマンドを発行する
	//
	_, err = CommandInitialSetup(0x04).Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x205f, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x205f {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandInitialSetup", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandInitialSetup: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// BルートPANA認証情報設定要求コマンドを発行する
	//
	_, err = CommandSetPanaAuthInfo(rbid, rbpassword).Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2054, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2054 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandSetPanaAuthInfo", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandSetPanaAuthInfo: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// アクティブスキャン要求コマンドを発行する
	//
	_, err = CommandActivescan(scanDuration, rbid).Write(stream)
	if err != nil {
		return err
	}
	// アクティブスキャン結果を受け取るチャネル(探しているのはスマートメーターなので1つあれば良い)
	foundBeaconChan := make(chan BeaconResponse, 1)
	defer close(foundBeaconChan)
	// アクティブスキャン通知を処理するゴルーチンを起動する
	go handleNotifyActivescan(ctx, rxNotifyChan, foundBeaconChan)
	// 応答コマンドコード:0x2051, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2051 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandActivescan", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandActivescan: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	// 検出したスマートメーターの情報
	var found BeaconResponse
	select {
	case found = <-foundBeaconChan:
		slog.Info("Found smartmeter", "beacon", found)

	case <-time.After(UartReadTimeout):
		return ErrUartReadTimeoutExceeded
	}

	// 設定ファイルに見つかったスマートメーターの情報を保存する
	settings := Settings{
		RouteBId:       string(rbid[:]),
		RouteBPassword: string(rbpassword[:]),
		Channel:        int(found.channel),
		MacAddress:     strconv.FormatUint(found.macAddress, 16),
		PanId:          int(found.panId),
	}
	jsonbytes, err := json.MarshalIndent(settings, "", strings.Repeat(" ", 2))
	if err != nil {
		slog.Error("MarshalIndent", "err", err)
		return err
	}

	err = os.WriteFile(settingsFileName, jsonbytes, 0644)
	if err != nil {
		slog.Error("WriteFile", "err", err)
		return err
	}

	slog.Info("Bye")

	return nil
}

// 0x4051: アクティブスキャン通知を処理する
func handleNotifyActivescan(ctx context.Context, rxNotify chan J11Datagram, found chan BeaconResponse) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-rxNotify:
			if r.Header.CommandCode == 0x4051 {
				// Data[0] = スキャン結果
				// Data[1] = スキャンチャネル
				// スキャン結果 = 0なら以下の情報が付加される
				// Data[2] = スキャン数
				// Data[3,4,5,6,7,8,9,10] = MACアドレス
				// Data[11,12] = PANID
				// Data[13] = rssi
				resultCode := r.Data[0]
				channel := r.Data[1]
				if resultCode == 0 {
					// Beacon応答あり
					macAddress := binary.BigEndian.Uint64(r.Data[3:11])
					panId := binary.BigEndian.Uint16(r.Data[11:13])
					rssi := int8(r.Data[13])
					// スマートメーターを検出した
					found <- BeaconResponse{
						channel:    channel,
						macAddress: macAddress,
						panId:      panId,
						rssi:       rssi,
					}
				}
				// Beacon応答無し
				slog.Debug("NotifyActivescan", "resultCode", resultCode, "channel", channel)
			}
		}
	}
}

// スマートメーターから電力消費量を得る
func run(settingsFileName string, serialName string) error {
	// 設定ファイルからスマートメーターの情報を得る
	jsonbytes, err := os.ReadFile(settingsFileName)
	if err != nil {
		slog.Error("ReadFile", "err", err)
		return err
	}
	//
	settings := Settings{}
	err = json.Unmarshal(jsonbytes, &settings)
	if err != nil {
		slog.Error("Unmarshal", "err", err)
		return err
	}
	var (
		routeBId       RouteBId       = [32]byte([]byte(settings.RouteBId))
		routeBPassword RouteBPassword = [12]byte([]byte(settings.RouteBPassword))
	)
	macAddress, err := strconv.ParseUint(settings.MacAddress, 16, 64)
	if err != nil {
		slog.Error("ParseUint", "err", err)
		return err
	}
	//
	config := &serial.Config{
		Name:        serialName,
		Baud:        115200,
		ReadTimeout: 10 * time.Second,
		Size:        8,
	}
	stream, err := serial.OpenPort(config)
	if err != nil {
		slog.Error("OpenPort", "err", err)
		return err
	}

	// コマンド応答チャネル
	rxDataChan := make(chan J11Datagram, 64)
	defer close(rxDataChan)
	// 通知チャネル
	rxNotifyChan := make(chan J11Datagram, 64)
	defer close(rxNotifyChan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go uartReceiver(ctx, stream, rxDataChan, rxNotifyChan)

	//
	// ハードウェアリセット要求コマンドを発行する
	//
	_, err = CommandHardwareReset().Write(stream)
	if err != nil {
		return err
	}
	// 起動完了通知: 0x6019を確認するまで待つ
	for done := false; !done; {
		select {
		case r := <-rxNotifyChan:
			done = r.Header.CommandCode == 0x6019
		case <-time.After(UartReadTimeout):
			return errors.New("J11 UART hardware reset command has no response")
		}
	}

	//
	// 初期設定要求コマンドを発行する
	//
	_, err = CommandInitialSetup(uint8(settings.Channel)).Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x205f, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x205f {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandInitialSetup", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandInitialSetup: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// BルートPANA認証情報設定要求コマンドを発行する
	//
	_, err = CommandSetPanaAuthInfo(routeBId, routeBPassword).Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2054, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2054 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandSetPanaAuthInfo", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandSetPanaAuthInfo: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// Bルート動作開始要求コマンドを発行する
	//
	_, err = CommandBRouteStart().Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2053, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2053 {
				done = true
				if r.Data[0] == 1 {
					var channel uint8 = r.Data[1]
					var panId uint16 = binary.BigEndian.Uint16(r.Data[2:4])
					var macAddress [8]byte = [8]byte(r.Data[4:12])
					var rssi int8 = int8(r.Data[12])
					slog.Debug("CommandBRouteStart",
						slog.String("result", "ok"),
						slog.Int("channel", int(channel)),
						slog.String("panId", strconv.FormatInt(int64(panId), 16)),
						slog.String("macAddress", hex.EncodeToString(macAddress[:])),
						slog.Int("rssi", int(rssi)),
					)
				} else {
					return fmt.Errorf("CommandBRouteStart: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// UDPポートオープン要求コマンドを発行する
	//
	_, err = CommandUdpPortOpen(0x0e1a).Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2005, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2005 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandUdpPortOpen", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandUdpPortOpen: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	//
	// BルートPANA開始要求コマンドを発行する
	//
	_, err = CommandBRouteStartPana().Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2056, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2056 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandBRouteStartPana", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandBRouteStartPana: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}
	// 0x6028: PANA認証結果通知を確認するまで待つ
	for done := false; !done; {
		select {
		case r := <-rxNotifyChan:
			if r.Header.CommandCode == 0x6028 {
				done = true
				result, macAddress := parseNotifyPanaResult(r)
				switch result {
				case 1: // 認証成功
					slog.Info("connection successful",
						slog.String("macAddress", hex.EncodeToString(macAddress[:])),
					)
				case 2: // 認証失敗
					return errors.New("PANA auth failed")
				case 3: // 応答なし
					return errors.New("no response to smart meter")
				default: // 規定の無いコード
					return fmt.Errorf("PANA auth failed:%v", result)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	// MACアドレスからIPv6リンクローカルアドレスへ変換する
	// MACアドレスの最初の1バイト下位2bit目を反転して
	// 0xFE80000000000000XXXXXXXXXXXXXXXXのXXをMACアドレスに置き換える
	address16 := [16]byte{}
	binary.BigEndian.PutUint64(address16[0:8], 0xFE80_0000_0000_0000)
	binary.BigEndian.PutUint64(address16[8:16], macAddress^0x0200_0000_0000_0000)
	ipv6address := netip.AddrFrom16(address16)

	// データ送信関数
	transmit := func(c *ConnEchonetlite, b []byte) error {
		_, err := c.Write(b)
		if err != nil {
			return err
		}
		// 応答コマンドコード:0x2008, 結果コード:0x01を確認する
		for done := false; !done; {
			select {
			case r := <-rxDataChan:
				if r.Header.CommandCode == 0x2008 {
					done = true
					if r.Data[0] == 1 {
						slog.Debug("Write",
							slog.String("transmit result", strconv.FormatInt(int64(r.Data[1]), 16)),
							slog.String("data digest", hex.EncodeToString(r.Data[2:])))
					} else {
						return fmt.Errorf("Write: %#v", r)
					}
				}
			case <-time.After(UartReadTimeout):
				return ErrUartReadTimeoutExceeded
			}
		}
		return nil
	}
	// データ受信関数
	receive := func(c *ConnEchonetlite) {
		buffer := make([]byte, 1500) // 最大受信サイズはヘッダ部を含めて1361バイト
		n, err := c.Read(buffer)
		if err != nil {
			slog.Error("read", "err", err)
			return
		}
		frame, err := ParseEchonetliteFrame(buffer[:n])
		if err != nil {
			slog.Error("read", "err", err)
			return
		}
		frame.Show()
	}

	//
	conn := NewConnEchonetlite(stream, ipv6address, rxNotifyChan)

	// PANAセッション確立後のインスタンスリスト通知が送られてくるまで待つ
	receive(conn)

	// データを受信するゴルーチンを起動する
	go func() {
		for {
			receive(conn)
		}
	}()

	// あいさつ代わりにスマートメータの属性を取得してみる
	if true {
		elSmartmeterProps := []EchonetliteEdata{
			// go言語では初期値0なのでpdc,edtは省略する
			{epc: 0x80}, // 動作状態
			{epc: 0x88}, // 異常発生状態
			{epc: 0x8a}, // メーカーコード
			{epc: 0xd3}, // 係数(存在しない場合は×1倍)
			{epc: 0xd7}, // 積算電力量有効桁数
			{epc: 0xe1}, // 積算電力量単位(正方向、逆方向計測値)
			{epc: 0xea}, // 定時積算電力量計測値(正方向計測値)
		}
		for _, item := range elSmartmeterProps {
			elFrame := EchonetliteFrame{
				ehd:   0x1081,
				tid:   0x0001,
				seoj:  [3]byte{0x05, 0xff, 0x01}, // home controller
				deoj:  [3]byte{0x02, 0x88, 0x01}, // smartmeter
				esv:   0x62,                      // get要求
				opc:   0x01,                      // 1つ
				edata: []EchonetliteEdata{item},
			}
			err := transmit(conn, elFrame.Encode())
			if err != nil {
				return err
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}

	// 今日の積算履歴を収集してみる
	if true {
		rqSetC := EchonetliteFrame{
			ehd:   0x1081,
			tid:   0x0001,
			seoj:  [3]byte{0x05, 0xff, 0x01},                               // home controller
			deoj:  [3]byte{0x02, 0x88, 0x01},                               // smartmeter
			esv:   0x61,                                                    // プロパティ値書き込み要求(応答要)
			opc:   0x01,                                                    // 1つ
			edata: []EchonetliteEdata{{epc: 0xe5, pdc: 1, edt: []byte{0}}}, // 積算履歴収集日1(edt=0は今日)
		}
		err := transmit(conn, rqSetC.Encode())
		if err != nil {
			return err
		}
		time.Sleep(1000 * time.Millisecond)
		rqGet := EchonetliteFrame{
			ehd:   0x1081,
			tid:   0x0001,
			seoj:  [3]byte{0x05, 0xff, 0x01},       // home controller
			deoj:  [3]byte{0x02, 0x88, 0x01},       // smartmeter
			esv:   0x62,                            // プロパティ値読み出し要求
			opc:   0x01,                            // 1つ
			edata: []EchonetliteEdata{{epc: 0xe2}}, // 積算電力量計測値履歴1
		}
		err = transmit(conn, rqGet.Encode())
		if err != nil {
			return err
		}
		time.Sleep(1000 * time.Millisecond)
	}

	// 積算電力量を得る
	err = transmit(conn, getElCumlativeWattHour())
	if err != nil {
		return err
	}
	//
	s := "waiting"
	for i := 0; i < 30; i++ {
		for k := 0; k < 5; k++ {
			fmt.Printf("%s%s\r", s, strings.Repeat(".", k))
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Printf("%s%s\r", s, strings.Repeat(" ", 5))
	}

	for count := 0; count < 3; count++ {
		// 瞬時電力と瞬時電流を得る
		err := transmit(conn, getElInstantWattAmpere())
		if err != nil {
			return err
		}
		//
		for i := 0; i < 30; i++ {
			for k := 0; k < 5; k++ {
				fmt.Printf("%s%s\r", s, strings.Repeat(".", k))
				time.Sleep(200 * time.Millisecond)
			}
			fmt.Printf("%s%s\r", s, strings.Repeat(" ", 5))
		}
	}
	fmt.Printf("\n")

	//
	// BルートPANA終了要求コマンドを発行する
	//
	_, err = CommandBRouteTerminatePana().Write(stream)
	if err != nil {
		return err
	}
	// 応答コマンドコード:0x2057, 結果コード:0x01を確認する
	for done := false; !done; {
		select {
		case r := <-rxDataChan:
			if r.Header.CommandCode == 0x2057 {
				done = true
				if r.Data[0] == 1 {
					slog.Debug("CommandBRouteTerminatePana", slog.String("result", "ok"))
				} else {
					return fmt.Errorf("CommandBRouteTerminatePana: %#v", r)
				}
			}
		case <-time.After(UartReadTimeout):
			return ErrUartReadTimeoutExceeded
		}
	}

	slog.Info("Bye")

	return nil
}

// 0x6028: PANA認証結果通知を処理する
func parseNotifyPanaResult(r J11Datagram) (uint8, [8]byte) {
	result := r.Data[0]
	macAddress := [8]byte(r.Data[1:9])
	return result, macAddress
}

// UDPポート0e1a(Echonet lite)に入出力する仕掛け
type ConnEchonetlite struct {
	stream            io.Writer
	ipv6              netip.Addr
	rxNotifyChan      chan J11Datagram
	senderAddress     netip.Addr
	senderPort        uint16
	dstPort           uint16
	panId             uint16
	senderAddressType uint8
	secure            uint8
	rssi              int8
	dataBytes         uint16
	data              []byte
}

func NewConnEchonetlite(w io.Writer, address netip.Addr, rxNotify chan J11Datagram) *ConnEchonetlite {
	return &ConnEchonetlite{stream: w, ipv6: address, rxNotifyChan: rxNotify}
}

func (c *ConnEchonetlite) Read(b []byte) (int, error) {
	r := J11Datagram{}
	// データ受信通知: 0x6018を確認するまでブロック
	for r = <-c.rxNotifyChan; r.Header.CommandCode != 0x6018; {
		slog.Debug("ignored", "rxNotify", r)
	}
	// Data[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15] = 送信元IPv6 アドレス
	// Data[16,17] = 送信元ポート番号
	// Data[18,19] = 送信先ポート番号
	// Data[20,21] = 送信元PAN ID
	// Data[22] = 送信先アドレス種別
	// Data[23] = 暗号化
	// Data[24] = RSSI
	// Data[25,26] = 受信データサイズ
	// Data[27:] = 受信データ
	c.senderAddress = netip.AddrFrom16([16]byte(r.Data[0:16]))
	c.senderPort = binary.BigEndian.Uint16(r.Data[16:18])
	c.dstPort = binary.BigEndian.Uint16(r.Data[18:20])
	c.panId = binary.BigEndian.Uint16(r.Data[20:22])
	c.senderAddressType = r.Data[22]
	c.secure = r.Data[23]
	c.rssi = int8(r.Data[24])
	c.dataBytes = binary.BigEndian.Uint16(r.Data[25:27])
	c.data = r.Data[27:]
	return copy(b, c.data), nil
}

func (c *ConnEchonetlite) Write(b []byte) (int, error) {
	// データ送信要求コマンドを発行する
	j11command, err := CommandTransmitData(c.ipv6, b)
	if err != nil {
		return 0, err
	}
	return j11command.Write(c.stream)
}

// UART通信読み取り
func uartReceiver(ctx context.Context, rd io.Reader, rxData chan J11Datagram, rxNotify chan J11Datagram) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			resp, err := readJ11ProtocolDatagram(ctx, rd)
			if err != nil {
				slog.Error("readJ11ProtocolDatagram", "err", err)
				continue
			}
			if resp == nil {
				continue
			}
			if 0x2000 <= resp.Header.CommandCode && resp.Header.CommandCode <= 0x2fff {
				rxData <- *resp // コマンド応答チャンネルへ送る
			} else {
				rxNotify <- *resp // 通知チャンネルへ送る
			}
		}
	}
}

func readJ11ProtocolDatagram(ctx context.Context, rd io.Reader) (*J11Datagram, error) {
	// d0 f9 ee 5d が検出できるまで入力を破棄し続ける
	var preamble uint32
	for preamble != UniqueCodeResponseCommand {
		var b [1]byte
		_, err := rd.Read(b[:])
		if err == io.EOF { // 読み取りデータ不足
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				continue
			}
		} else if err != nil {
			return nil, err
		}
		preamble = preamble<<8 | uint32(b[0])
	}
	// ヘッダ部読み取り
	var buf [J11DatagramHeaderBytes]byte
	binary.BigEndian.PutUint32(buf[:], preamble)
	for i := 4; i < J11DatagramHeaderBytes; {
		n, err := rd.Read(buf[i:])
		if err == io.EOF { // 読み取りデータ不足
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				continue
			}
		} else if err != nil {
			return nil, err
		}
		i += n
	}
	header := J11DatagramHeader{}
	binary.Decode(buf[:], binary.BigEndian, &header)
	// ヘッダ部チェックサム検査
	if header.HeaderChecksum != header.CalcHeaderChecksum() {
		slog.Debug(
			"header checksum mismatched",
			"checksum", header.CalcHeaderChecksum(),
			"HeaderChecksum", header.HeaderChecksum,
		)
		return nil, nil
	}
	// データ部読み取り
	dataBytes := header.MessageLen - 4
	data := make([]byte, dataBytes)
	for i := 0; i < int(dataBytes); {
		n, err := rd.Read(data[i:])
		if err == io.EOF { // 読み取りデータ不足
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				continue
			}
		} else if err != nil {
			return nil, err
		}
		i += n
	}
	// データ部チェックサム検査
	if header.DataChecksum != CalcChecksum(data) {
		slog.Debug(
			"data checksum mismatched",
			"checksum", CalcChecksum(data),
			"DataChecksum", header.DataChecksum,
		)
		return nil, nil
	}

	return &J11Datagram{Header: header, Data: data}, nil
}

func main() {
	var (
		settingsFileName string
		serialDevice     string
		rbid             RouteBId
		rbpassword       RouteBPassword
		scanDuration     int
	)
	app := &cli.App{
		Name:    "BRouteJ11",
		Usage:   "BP35Cx-J11を使ってスマートメータから電力消費量などを得る",
		Version: "1.0.0",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "settings",
				Aliases:     []string{"S"},
				Usage:       "設定ファイル名",
				Destination: &settingsFileName,
				Value:       "settings.json",
			},
			&cli.StringFlag{
				Name:        "device",
				Aliases:     []string{"D"},
				Usage:       "シリアルデバイス名",
				Destination: &serialDevice,
				Value:       "/dev/ttyUSB0",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "pairing",
				Usage: "ペアリングして情報を設定ファイルに保存する",
				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:        "activescan",
						Aliases:     []string{"T"},
						Usage:       "アクティブスキャン時間(1～14)",
						Destination: &scanDuration,
						Value:       7,
					},
					&cli.StringFlag{
						Name:    "id",
						Aliases: []string{"Id"},
						Usage:   "ルートBID(32文字)",
						Action: func(ctx *cli.Context, s string) error {
							bytes := []byte(s)
							if len(bytes) != 32 {
								return fmt.Errorf("ルートＢＩＤは32文字です")
							}
							rbid = [32]byte(bytes)
							return nil
						},
					},
					&cli.StringFlag{
						Name:    "password",
						Aliases: []string{"Pwd"},
						Usage:   "ルートBパスワード(12文字)",
						Action: func(ctx *cli.Context, s string) error {
							bytes := []byte(s)
							if len(bytes) != 12 {
								return fmt.Errorf("ルートＢパスワードは12文字です")
							}
							rbpassword = [12]byte(bytes)
							return nil
						},
					},
				},
				Action: func(c *cli.Context) error {
					slog.SetDefault(
						slog.New(
							slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))
					err := pairing(settingsFileName, serialDevice, uint8(scanDuration), rbid, rbpassword)
					if err != nil {
						return err
					}
					return nil
				},
			},
			{
				Name:  "run",
				Usage: "スマートメータから電力消費量を得る",
				Flags: []cli.Flag{},
				Action: func(c *cli.Context) error {
					slog.SetDefault(
						slog.New(
							slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))
					err := run(settingsFileName, serialDevice)
					if err != nil {
						return err
					}
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("app.Run", "err", err)
		return
	}
}
