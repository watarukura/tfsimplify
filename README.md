# tfsimplify

Terraform の `.tf` ファイルから、プロバイダースキーマのデフォルト値と同じ値が設定されている属性を自動的に削除する CLI ツールです。
不要な記述を取り除くことで、Terraform コードをシンプルに保つことができます。

## 特徴

- AWS プロバイダーのスキーマ情報を `terraform providers schema -json` から取得し、デフォルト値を判定
- `optional` かつ非 `computed` な属性のみを対象とし、安全に削除
- `--write` でファイルを直接書き換え、`--check` で CI での差分チェックに対応
- リテラル値のみを対象とし、変数参照や関数呼び出しを含む式は変更しない

## 前提条件

- [Go](https://go.dev/) 1.25 以上
- [Terraform](https://www.terraform.io/) CLI がインストール済みであること
- 対象ディレクトリで `terraform init` が実行済みであること

## インストール

```bash
go install github.com/watarukura/tfsimplify@latest
```

## 使い方

`-dir` オプションは必須です。指定しない場合は以下の usage が表示されます。

```
Usage: tfsimplify -dir <directory> [options]

tfsimplify removes attributes from Terraform .tf files that match
the provider schema's default values.

Options:
  -check
    	exit 1 if changes are needed (no write)
  -dir string
    	directory to scan for .tf files (required)
  -write
    	rewrite files in-place

Examples:
  tfsimplify -dir ./environments/prod
  tfsimplify -dir ./environments/prod --write
  tfsimplify -dir ./environments/prod --check
```

```bash
# 指定ディレクトリの .tf ファイルを解析し、変更が必要なファイルを表示
tfsimplify -dir ./environments/prod

# ファイルを直接書き換える
tfsimplify -dir ./environments/prod --write

# CI 用: 変更が必要な場合に exit 1 を返す
tfsimplify -dir ./environments/prod --check
```

## 例

以下のような Terraform コードがある場合:

```hcl
resource "aws_s3_bucket" "example" {
  bucket              = "example"
  bucket_prefix       = null
  force_destroy       = false
  object_lock_enabled = false
  tags                = {}
}
```

`tfsimplify --write` を実行すると、`optional` かつ非 `computed` な属性のうち、デフォルト値と一致する `force_destroy = false` と `tags = {}` が削除されます。
`bucket_prefix = null` は null 値のため安全のために保持されます。
`object_lock_enabled = false` は `computed` 属性のため安全のために保持されます。

```hcl
resource "aws_s3_bucket" "example" {
  bucket              = "example"
  bucket_prefix       = null
  object_lock_enabled = false
}
```

### 無視したい行の指定

tfsimplify-ignore 指定の直下の行は対象外とします。

```hcl
resource "aws_s3_bucket" "example" {
  bucket              = "example"
  bucket_prefix       = null
  # tfsimplify-ignore
  force_destroy       = false
  object_lock_enabled = false
  tags                = {}
}
```


tfsimplify-disable と tfsimplify-enable で挟まれた行は対象外とします。

```hcl
resource "aws_s3_bucket" "example" {
  bucket              = "example"
  bucket_prefix       = null
  # tfsimplify-disable
  force_destroy       = false
  # tfsimplify-enable
  object_lock_enabled = false
  tags                = {}
}
```

## オプション

| フラグ     | デフォルト | 説明                                           |
| ---------- | ---------- | ---------------------------------------------- |
| `-dir`     | (必須)     | `.tf` ファイルを検索するディレクトリ           |
| `--write`  | `false`    | ファイルを直接書き換える                       |
| `--check`  | `false`    | 変更が必要な場合に exit code 1 を返す (CI 向け) |

> `--write` と `--check` は同時に指定できません。

## ライセンス

MIT
