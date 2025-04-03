load("//tools:build_rules/shims.bzl", "go_binary")

package(default_visibility = ["//kythe:default_visibility"])

go_binary(
    name = "rust_project_to_kzip",
    srcs = ["rust_project_to_kzip.go"],
    deps = ["//kythe/go/util/log"],
)
