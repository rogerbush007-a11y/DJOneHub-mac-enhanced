module github.com/iniwex5/vohive

go 1.26.3

replace github.com/damonto/euicc-go => ./third_party/euicc-go

replace github.com/damonto/uicc-go => ./third_party/uicc-go

replace github.com/iniwex5/quectel-qmi-go => ./third_party/quectel-qmi-go

replace github.com/lestrrat-go/strftime => ./third_party/strftime

replace github.com/pkg/errors => ./third_party/pkg-errors

replace golang.org/x/sys => ./third_party/x-sys

replace golang.org/x/text => ./third_party/x-text

replace go.uber.org/multierr => ./third_party/multierr

require (
	github.com/damonto/euicc-go v1.1.3-0.20260628013808-8d873a2dfc98
	github.com/damonto/uicc-go v0.0.0-20260629073618-7ddada6bb13e
	github.com/iniwex5/quectel-qmi-go v0.6.0
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible
	github.com/spf13/viper v1.21.0
	github.com/warthog618/sms v0.3.0
	go.bug.st/serial v1.6.4
	go.uber.org/zap v1.27.1
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.46.0
)

require (
	github.com/creack/goselect v0.1.2 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/iniwex5/netlink v1.3.3 // indirect
	github.com/lestrrat-go/strftime v1.2.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/text v0.28.0 // indirect
)
