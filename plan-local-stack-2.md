# plan: local-stack-2 (残課題全対応)

- [x] 引数処理: 未知フラグを gopls に素通し、GOPLS_FLEET_* 環境変数対応
- [x] eviction: open 参照カウント + TTL で scope 縮小
- [x] crossref v2: definition 解決 → 定義パッケージで閉包、method 宣言なら一時 full scope
- [x] go:embed をシグネチャに含めて graph cache 無効化
- [x] 単体テスト追加、go test -race green
- [x] layerone 機能確認 (references 非 method 4.5s / method 9.3s warm、full 拡大は
      directoryFilters 明示セットの修正で reload が発火するようになった)
- [x] README にエディタ設定手順 (VS Code / Neovim)、commit + merge
