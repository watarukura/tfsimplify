以下は、これまでの内容を整理した **Terraform attribute default 削除 linter の設計書（Markdown）** です。
初期実装にフォーカスした **ミニマル設計**にしています。

---

# Terraform Default Attribute Simplifier 設計書

## 1. 概要

Terraform の `resource` / `data` ブロックにおいて、
**provider の default 値と同じ attribute を削除する** Linter / Formatter を実装する。

これにより以下を目的とする。

* Terraform コードの冗長性削減
* provider default に依存したシンプルな記述
* コードレビュー時のノイズ削減

例:

```hcl
resource "aws_s3_bucket" "example" {
  force_destroy = false
}
```

provider の default が `false` の場合、以下へ変換される。

```hcl
resource "aws_s3_bucket" "example" {}
```

---

# 2. スコープ

## 2.1 対象

Terraform `.tf` ファイルの以下のブロック。

```
resource
data
```

対象 provider:

```
terraform providers schema -json で取得できるすべての provider
```

---

## 2.2 初期実装の制限

安全性を優先するため、以下の制限を設ける。

| 制限                            | 理由                                  |
| ----------------------------- | ----------------------------------- |
| module ブロックは対象外               | module variable default の解析が必要になるため |
| nested block は対象外             | 再帰解析が必要                             |
| attribute 値はリテラルのみ            | 式評価を避けるため                           |
| Optional && !Computed のみ      | Computed を削除すると意味が変わる可能性            |
| default が明示されている attribute のみ | default 不明な場合は削除不可                  |
| null 値は削除しない                  | Terraform の意味が変わる可能性                |

---

# 3. システム構成

ツールは以下のコンポーネントで構成される。

```
CLI
 │
 ├─ Terraform Init Check
 │
 ├─ Provider Schema Loader
 │
 ├─ HCL Parser
 │
 ├─ Attribute Comparator
 │
 └─ File Rewriter
```

---

# 4. 処理フロー

全体フロー。

```
start
  │
  ├─ 対象ディレクトリ取得
  │
  ├─ terraform init 済み確認
  │
  ├─ terraform providers schema -json 実行
  │
  ├─ 全 provider schema 抽出
  │
  ├─ .tf ファイル探索
  │
  ├─ 各ファイルを HCL AST にパース
  │
  ├─ resource / data ブロック走査
  │
  ├─ attribute ごとに
  │      │
  │      ├─ schema attribute 取得
  │      ├─ default 取得
  │      ├─ literal 判定
  │      ├─ default と比較
  │      └─ 一致 → attribute 削除
  │
  ├─ 差分検出
  │
  ├─ --write なら書き込み
  │
  └─ end
```

---

# 5. Terraform 初期化チェック

本ツールは **Terraform working directory** を対象とする。

実行前に以下をチェックする。

```
.terraform/
.terraform.lock.hcl
```

### 未初期化時

```
terraform is not initialized
run: terraform init
```

---

# 6. Provider Schema 取得

各 provider の attribute default を取得するため、

```
terraform providers schema -json
```

を実行する。

取得対象:

```
provider_schemas 内のすべての provider を対象とする
```

必要な情報:

```
resource_schemas
data_source_schemas
```

各 attribute の以下のフィールドを使用。

```
optional
computed
default
```

---

# 7. HCL パース

`.tf` ファイルは以下ライブラリで解析する。

```
github.com/hashicorp/hcl/v2
github.com/hashicorp/hcl/v2/hclwrite
```

使用目的

| ライブラリ     | 用途       |
| --------- | -------- |
| hclwrite  | 編集可能 AST |
| hclsyntax | 式の解析     |

---

# 8. Attribute 削除条件

以下すべてを満たす場合のみ削除する。

### 1. schema attribute 存在

```
attribute ∈ provider schema
```

---

### 2. Optional && !Computed

```
Optional == true
Computed == false
```

---

### 3. default が存在

```
default != null
```

---

### 4. HCL 値がリテラル

対象:

```
string
number
bool
list
map
object
```

対象外:

```
var.*
local.*
module.*
function()
conditional
template
```

---

### 5. null は削除対象外

```
attribute = null
```

は削除しない。

---

### 6. default と一致

JSON レベルで比較する。

例:

```
true == true
1 == 1
"a" == "a"
```

---

# 9. ファイル書き換え

削除は

```
hclwrite.Body.RemoveAttribute()
```

を使用する。

---

## 9.1 書き込みモード

CLI オプションで制御する。

| option  | 動作     |
| ------- | ------ |
| --check | 差分検出のみ |
| --write | ファイル更新 |

---

### check モード

```
exit 1 → 差分あり
exit 0 → 差分なし
```

CI 用。

---

# 10. CLI 仕様

```
tfsimplify [dir]
```

デフォルト

```
dir = .
```

---

### オプション

```
--check
--write
```

---

### 例

チェック

```
tfsimplify . --check
```

書き込み

```
tfsimplify . --write
```

---

# 11. ディレクトリ探索

`.tf` ファイルを再帰探索する。

除外ディレクトリ

```
.terraform
.git
```

---

# 12. エラーハンドリング

| ケース                 | 対応   |
| ------------------- | ---- |
| terraform init 未実行  | エラー  |
| schema 取得失敗         | エラー  |
| schema attribute 不明 | スキップ |
| default 不明          | スキップ |
| 非リテラル式              | スキップ |

---

# 13. 将来拡張

初期版では対象外だが、将来対応予定。

---

## module default 最適化

```
module "x" {
  variable = default
}
```

module variable default と比較して削除。

---

## nested block

例:

```
ingress {}
lifecycle {}
timeouts {}
```

---

## expression evaluation

```
1 + 1
concat()
ternary
```

---

## provider 自動検出

```
terraform providers schema -json で取得できるすべての provider を自動検出（実装済み）
```

---

## diff 出力

```
--diff
```

追加予定。

---

# 14. テスト戦略

テスト方式

```
golden test
```

例

```
input.tf
expected.tf
```

---

## テストケース

| ケース                  | 内容             |
| -------------------- | -------------- |
| default 削除           | true → default |
| default 不一致          | 保持             |
| null 値               | 保持             |
| computed attribute   | 保持             |
| expression attribute | 保持             |

---

# 15. ディレクトリ構成

例

```
tfsimplify
 ├ main.go
 ├ schema.go
 ├ parser.go
 ├ simplify.go
 ├ go.mod
 └ testdata
      ├ case1
      │   ├ input.tf
      │   └ expected.tf
```

---

# 16. まとめ

本ツールは以下を実現する。

* Terraform provider schema を利用した **安全な default attribute 削除**
* すべての Terraform provider に対応
* `terraform init` 済み環境で動作
* CI で利用可能な **check モード**

初期実装は **安全性優先の最小スコープ**とし、
将来的に以下を拡張する。

* module variable default
* nested block
* diff 表示

---
