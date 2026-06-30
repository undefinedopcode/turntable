// sysinfo is a standalone module (like any real external plugin would be). It
// depends only on the turntable plugin SDK, resolved locally via the replace
// directive while in this repo.
module github.com/april/turntable/examples/plugins/sysinfo

go 1.23

require github.com/april/turntable/sdk/go v0.0.0

replace github.com/april/turntable/sdk/go => ../../../sdk/go
