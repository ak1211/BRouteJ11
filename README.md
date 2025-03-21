# BRouteJ11
スマートメーターのルートBにBP35Cx-J11-T01やBP35C2-J11-T01で通信して  
瞬時電力他を得るアプリケーション。 
ラズパイ３で確認しました。

## ビルド方法
何らかの方法でgo言語をインストールする。

$ go build

## 使い方
BP35C2-J11-T01等をPC(またはラズパイ)にUSB(またはシリアル)で接続する 

## 接続するスマートメータを探す
$ BRouteJ11 pairing --id "000000xxxxxxxxxxxxxxxxxxxxxxxxxx" --password "xxxxxxxxxxxx"

成功すると接続情報がsettings.jsonに保存される。

## スマートメータから瞬時電力を得る
$ BRouteJ11 run

## License
Licensed under the MIT License.  
See LICENSE file in the project root for full license information.

