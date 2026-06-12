# plan: local-stack (Phase 2 完成 + Phase 3)

クラウド層なし、ローカル完結。layerone 実測で締める。

- [x] 2 段階 rescope: didOpen 即フォワード + publishDiagnostics 観測後に rescope
- [x] reverse import index: ImportsOnly 全スキャン + rename/references の hold & forward
- [x] GOPACKAGESDRIVER: unix socket デーモン + go/packages キャッシュ + NotHandled フォールバック
- [x] 単体テスト (scope unit / 逆依存閉包 / driver パターン判定)
- [x] layerone 実測: 初回診断 1.4s / rescope 12.8s → 4.3s / references 3.8s で正しくスコープ拡大
- [x] README / report.md 更新、commit
