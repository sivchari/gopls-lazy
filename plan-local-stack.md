# plan: local-stack (Phase 2 完成 + Phase 3)

クラウド層なし、ローカル完結。layerone 実測で締める。

- [ ] 2 段階 rescope: didOpen 即フォワード + publishDiagnostics 観測後に rescope
- [ ] reverse import index: ImportsOnly 全スキャン + rename/references の hold & forward
- [ ] GOPACKAGESDRIVER: unix socket デーモン + go/packages キャッシュ + NotHandled フォールバック
- [ ] 単体テスト (scope unit / 逆依存閉包 / driver パターン判定)
- [ ] layerone 実測: rescope 12.8s 短縮 / 初回診断 / references スコープ拡大
- [ ] README / report.md 更新、commit
