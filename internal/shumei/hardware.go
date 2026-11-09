package shumei

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// Hardware is the device-specific half of the device identity this client
// reports: the fields a real phone derives from its build, SoC, screen and
// sensors. It drives both the ShuMei `aNN` fingerprint (deviceData fills the
// a10/a32/a33/a34/a36/a37/a45/a46/a47/a48/a56/a57/a143/a154/a18/a131/a132
// slots from it) and the Login's device fields (auth.BuildLoginParams turns it
// into phone_model, osVersion, device_string, mt, AndroidID and IMEI), so both
// describe the same phone.
//
// Field letters and Login field names cite the SDK's name table
// (l111l1111lI1l.java), the collector's getters (l111l1111llIl.java) and the
// game's own com.sdkmanager.utils.Device/Udid and Assembly-CSharp DeviceManager.
// A catalog entry is only valid when its values agree with one another: the
// model, brand, board, fingerprint, SoC, screen and RAM must all describe the
// same phone, the way Build/build.prop would on the real device.
type Hardware struct {
	AndroidVersion string            // a10, Build.VERSION.RELEASE ("12")
	APILevel       string            // Build.VERSION.SDK_INT ("31")
	Screen         string            // a36, "width,height,dpi"; a92 is the "width,height" prefix
	DeviceName     string            // a45, Build.DEVICE
	SoC            string            // a143/a33, ro.board.platform (ro.hardware)
	MaxFreq        int               // a34, CPU max frequency in kHz
	Cores          int               // a32, CPU core count
	RAMBytes       int64             // a48, total RAM in bytes
	StorageGB      int               // a37, storage in GB
	CertSubject    string            // a56, the self-signed cert's DN
	CertHash       int               // a57, its hash
	Build          map[string]string // a46: board, model, brand, manufacturer, fingerprint, cpu_abi, cpu_abi2, radioVersion
	Props          map[string]string // a18: ro.debuggable, ro.boot.hardware, gsm.sim.state, gsm.operator.alpha, sys.usb.state
	Sensors        []string          // a47: "index,vendor"
	Launcher       map[string]string // a154: pkg, label, versionName
	Locale         string            // a131/a132
	GPUModel       string            // SystemInfo.graphicsDeviceName
	GPUVendor      string            // SystemInfo.graphicsDeviceVendor
	SimOperator    string            // TelephonyManager.getSimOperator(): MCC+MNC
	Serial         string            // Build.SERIAL (the Login's misnamed "AndroidID" field)
}

// Model returns Build.MODEL, SystemInfo.deviceModel, and the Login's
// phone_model: the phone's marketing model string.
func (h Hardware) Model() string { return h.Build["model"] }

// OSVersionString returns GetOSVersion(): SystemInfo.operatingSystem, Unity's
// "Android OS <release> / API-<sdk> (<fingerprint>)" string.
func (h Hardware) OSVersionString() string {
	return fmt.Sprintf("Android OS %s / API-%s (%s)", h.AndroidVersion, h.APILevel, h.Build["fingerprint"])
}

// DeviceString returns GetDeviceString(), the "Model=..|Memory=..|Vendor=..|
// Processor=..|Graphics=..|DeviceType=..|" dump. Memory is Unity's
// systemMemorySize in MiB and Processor is systemProcessorFrequency in MHz; the
// DeviceType is getGraphicsDeviceType()'s OpenGL level plus the texture-format
// suffixes a real ES 3.2 device supports.
func (h Hardware) DeviceString() string {
	return fmt.Sprintf("Model=%s|Memory=%d|Vendor=%s|Processor=%d|Graphics=%s|DeviceType=OpenGL3.2-ASTC-ETC2-ASTC_4x4-ASTC_6x6|",
		h.Model(), h.RAMBytes/(1024*1024), h.GPUVendor, h.MaxFreq/1000, h.GPUModel)
}

// HandSetInfo returns getHandSetInfo(), the "Model:..,SDKVersion:..,SYSVersion:..,
// Brand:..,SimProvider:..,NetWork:.." dump sent as the Login's mt field.
func (h Hardware) HandSetInfo() string {
	return fmt.Sprintf("Model:%s,SDKVersion:%s,SYSVersion:%s,Brand:%s,SimProvider:%s,NetWork:Wifi",
		h.Model(), h.APILevel, h.AndroidVersion, h.Build["brand"], h.SimOperator)
}

// DeviceInfo returns Udid.getDeviceInfo(), the space-separated build dump the
// real client sends in the Login's misnamed "IMEI" field: board, hardware,
// brand, cpu_abi, cpu_abi2, device, manufacturer, serial, product, width,
// height, dpi, fingerprint, manufacturer, model.
func (h Hardware) DeviceInfo() string {
	w, hgt, dpi := h.screenParts()
	return strings.Join([]string{
		h.Build["board"], h.Props["ro.boot.hardware"], h.Build["brand"],
		h.Build["cpu_abi"], h.Build["cpu_abi2"], h.DeviceName,
		h.Build["manufacturer"], h.Serial, h.product(),
		strconv.Itoa(w), strconv.Itoa(hgt), strconv.Itoa(dpi),
		h.Build["fingerprint"], h.Build["manufacturer"], h.Model(),
	}, " ") + " "
}

// product returns Build.PRODUCT (ro.product.name), the second component of the
// build fingerprint (brand/product/device:...).
func (h Hardware) product() string {
	parts := strings.SplitN(h.Build["fingerprint"], "/", 3)
	if len(parts) < 2 {
		return h.DeviceName
	}
	return parts[1]
}

// screenParts returns Screen's width, height and dpi as ints.
func (h Hardware) screenParts() (w, hgt, dpi int) {
	parts := strings.Split(h.Screen, ",")
	if len(parts) != 3 {
		return 0, 0, 0
	}
	w, _ = strconv.Atoi(parts[0])
	hgt, _ = strconv.Atoi(parts[1])
	dpi, _ = strconv.Atoi(parts[2])
	return w, hgt, dpi
}

// ScreenWH returns Screen's width and height in pixels, for the Login's
// analytics blob and phone_screen fields.
func (h Hardware) ScreenWH() (w, height int) {
	w, height, _ = h.screenParts()
	return w, height
}

// hardwareCatalog is a set of internally-consistent real Android 12 device
// signatures. Each one's build fingerprint, board, SoC, screen, RAM and sensor
// list all belong to the same phone, so the fingerprint reads as one real
// device rather than a patchwork. The build IDs are Android 12 (SP1A.*),
// matching androidVersion "12". Model numbers, SoC platforms, RAM, storage,
// screen resolutions and CPU clocks are the devices' published specifications;
// serials are plausible Build.SERIAL-shaped strings.
var hardwareCatalog = []Hardware{
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1440,3200,511", DeviceName: "z3q", SoC: "exynos990",
		MaxFreq: 2730000, Cores: 8, RAMBytes: 12884901888, StorageGB: 256,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "z3q", "model": "SM-G988B", "brand": "samsung", "manufacturer": "samsung",
			"fingerprint": "samsung/z3qksx/z3q:12/SP1A.210812.016/G988NKSU3FVK1:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "G988NKSU3FVK1,G988NKSU3FVK1",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "exynos990", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "SKTelecom", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "2,AKM", "4,STMicroelectronics", "5,ams", "6,STMicroelectronics"},
		Launcher: map[string]string{
			"pkg": "com.sec.android.app.launcher", "label": "One UI Home", "versionName": "14.5.01.7",
		},
		Locale:   "ko",
		GPUModel: "Mali-G77 MP11", GPUVendor: "ARM", SimOperator: "45005", Serial: "RF8M12345AB",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2400,421", DeviceName: "o1s", SoC: "exynos2100",
		MaxFreq: 2900000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "o1s", "model": "SM-G991B", "brand": "samsung", "manufacturer": "samsung",
			"fingerprint": "samsung/o1sxeea/o1s:12/SP1A.210812.016/G991BXXU4CVD2:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "G991BXXU4CVD2,G991BXXU4CVD2",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "exynos2100", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "Play", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "2,AKM", "4,STMicroelectronics", "5,ams", "6,STMicroelectronics"},
		Launcher: map[string]string{
			"pkg": "com.sec.android.app.launcher", "label": "One UI Home", "versionName": "14.1.01.7",
		},
		Locale:   "en",
		GPUModel: "Mali-G78 MP14", GPUVendor: "ARM", SimOperator: "26006", Serial: "R58M67890CD",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2340,425", DeviceName: "r0s", SoC: "exynos2200",
		MaxFreq: 2803000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "r0s", "model": "SM-S901B", "brand": "samsung", "manufacturer": "samsung",
			"fingerprint": "samsung/r0sxeea/r0s:12/SP1A.210812.016/S901BXXU1AVC6:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "S901BXXU1AVC6,S901BXXU1AVC6",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "exynos2200", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "EE", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "2,AKM", "4,STMicroelectronics", "5,ams", "6,STMicroelectronics"},
		Launcher: map[string]string{
			"pkg": "com.sec.android.app.launcher", "label": "One UI Home", "versionName": "14.0.02.10",
		},
		Locale:   "en",
		GPUModel: "ANGLE (Samsung Xclipse 920)", GPUVendor: "Samsung Electronics Co., Ltd.", SimOperator: "23410", Serial: "RF8N11223DE",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2400,411", DeviceName: "oriole", SoC: "gs101",
		MaxFreq: 2802000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "slider", "model": "Pixel 6", "brand": "google", "manufacturer": "Google",
			"fingerprint": "google/oriole/oriole:12/SD1A.210817.036/7805805:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "g5123b-92118-220125-B-8131543",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "slider", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "T-Mobile", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "4,Bosch", "9,Bosch", "13,ams"},
		Launcher: map[string]string{
			"pkg": "com.google.android.apps.nexuslauncher", "label": "Pixel Launcher", "versionName": "12",
		},
		Locale:   "en",
		GPUModel: "Mali-G78 MP20", GPUVendor: "ARM", SimOperator: "310260", Serial: "1A051FDT300123",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1440,3120,512", DeviceName: "raven", SoC: "gs101",
		MaxFreq: 2802000, Cores: 8, RAMBytes: 12884901888, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "raven", "model": "Pixel 6 Pro", "brand": "google", "manufacturer": "Google",
			"fingerprint": "google/raven/raven:12/SD1A.210817.036/7805805:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "g5123b-92118-220125-B-8131543",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "raven", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "AT&T", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "4,Bosch", "9,Bosch", "13,ams"},
		Launcher: map[string]string{
			"pkg": "com.google.android.apps.nexuslauncher", "label": "Pixel Launcher", "versionName": "12",
		},
		Locale:   "en",
		GPUModel: "Mali-G78 MP20", GPUVendor: "ARM", SimOperator: "310410", Serial: "1B061FDT400456",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2400,395", DeviceName: "amber", SoC: "mt6893",
		MaxFreq: 3000000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "amber", "model": "21081111RG", "brand": "Xiaomi", "manufacturer": "Xiaomi",
			"fingerprint": "Xiaomi/amber_global/amber:12/SP1A.210812.016/V13.0.2.0.SKWMIXM:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "MOLY.NR15.R3.TC36.PR2.SP.V1.P18",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "mt6893", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "Play", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,Bosch", "4,Bosch", "9,STMicroelectronics", "13,TDK"},
		Launcher: map[string]string{
			"pkg": "com.miui.home", "label": "System launcher", "versionName": "ALPHA-4.23.0.2350-12211819",
		},
		Locale:   "pl",
		GPUModel: "Mali-G77 MC9", GPUVendor: "ARM", SimOperator: "26006", Serial: "X01234567890",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1440,3200,515", DeviceName: "venus", SoC: "lahaina",
		MaxFreq: 2842000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "venus", "model": "M2011K2G", "brand": "Xiaomi", "manufacturer": "Xiaomi",
			"fingerprint": "Xiaomi/venus/venus:12/SP1A.210812.016/V13.0.4.0.SKBMIXM:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "MPSS.AT.4.1.c2-00046-SDX55_GEN_PACK-2",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "lahaina", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "Plus", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,Bosch", "4,Bosch", "9,STMicroelectronics", "13,AKM"},
		Launcher: map[string]string{
			"pkg": "com.miui.home", "label": "System launcher", "versionName": "RELEASE-4.26.0.2742-12111057",
		},
		Locale:   "pl",
		GPUModel: "Adreno (TM) 660", GPUVendor: "Qualcomm", SimOperator: "26001", Serial: "X12345678901",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2400,402", DeviceName: "OnePlus9", SoC: "lahaina",
		MaxFreq: 2842000, Cores: 8, RAMBytes: 12884901888, StorageGB: 256,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "lahaina", "model": "LE2115", "brand": "OnePlus", "manufacturer": "OnePlus",
			"fingerprint": "OnePlus/OnePlus9_NA/OnePlus9:12/SP1A.210812.016/L.202205122255:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "MONTAGE.M.TC3.SP.V1.P3.210929.1",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "lahaina", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "Verizon", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "4,Bosch", "9,Bosch", "13,AMS"},
		Launcher: map[string]string{
			"pkg": "net.oneplus.launcher", "label": "OnePlus Launcher", "versionName": "5.4.9.220608151019.99bc7a5",
		},
		Locale:   "en",
		GPUModel: "Adreno (TM) 660", GPUVendor: "Qualcomm", SimOperator: "311480", Serial: "OP5B7A9C3D",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1440,3088,496", DeviceName: "c1s", SoC: "exynos990",
		MaxFreq: 2730000, Cores: 8, RAMBytes: 12884901888, StorageGB: 256,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "c1s", "model": "SM-N986B", "brand": "samsung", "manufacturer": "samsung",
			"fingerprint": "samsung/c1sxeea/c1s:12/SP1A.210812.016/N986BXXU4FVC5:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "N986BXXU4FVC5,N986BXXU4FVC5",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "exynos990", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "EE", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "2,AKM", "4,STMicroelectronics", "5,ams", "6,STMicroelectronics"},
		Launcher: map[string]string{
			"pkg": "com.sec.android.app.launcher", "label": "One UI Home", "versionName": "14.5.01.7",
		},
		Locale:   "en",
		GPUModel: "Mali-G77 MP11", GPUVendor: "ARM", SimOperator: "23430", Serial: "RF8N9A1B2C3D",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2340,432", DeviceName: "redfin", SoC: "lito",
		MaxFreq: 2400000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "redfin", "model": "Pixel 5", "brand": "google", "manufacturer": "Google",
			"fingerprint": "google/redfin/redfin:12/SP1A.210812.015/7671068:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "g7250-00228.1-210810-B-7598812",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "redfin", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "T-Mobile", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,Bosch", "2,AKM", "4,Bosch", "5,ams"},
		Launcher: map[string]string{
			"pkg": "com.google.android.apps.nexuslauncher", "label": "Pixel Launcher", "versionName": "12",
		},
		Locale:   "en",
		GPUModel: "Adreno (TM) 620", GPUVendor: "Qualcomm", SimOperator: "310260", Serial: "1A051FDT500789",
	},
	{
		AndroidVersion: "12", APILevel: "31",
		Screen: "1080,2400,402", DeviceName: "OnePlus8T", SoC: "kona",
		MaxFreq: 2842000, Cores: 8, RAMBytes: 8589934592, StorageGB: 128,
		CertSubject: "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US", CertHash: -1802451281,
		Build: map[string]string{
			"board": "kebab", "model": "KB2003", "brand": "OnePlus", "manufacturer": "OnePlus",
			"fingerprint": "OnePlus/OnePlus8T_EEA/OnePlus8T:12/SP1A.210812.016/KB2003_11_F.13:user/release-keys",
			"cpu_abi":     "arm64-v8a", "cpu_abi2": "", "radioVersion": "MONTAGE.M.1.1.1.C2-00045-SDX55_GEN_PACK-2",
		},
		Props: map[string]string{
			"ro.debuggable": "0", "ro.boot.hardware": "kona", "gsm.sim.state": "READY",
			"gsm.operator.alpha": "EE", "sys.usb.state": "android,adb",
		},
		Sensors: []string{"1,STMicroelectronics", "2,AKM", "4,STMicroelectronics", "5,ams"},
		Launcher: map[string]string{
			"pkg": "net.oneplus.launcher", "label": "OnePlus Launcher", "versionName": "5.4.10.220407150435.64ec0c2",
		},
		Locale:   "en",
		GPUModel: "Adreno (TM) 650", GPUVendor: "Qualcomm", SimOperator: "23430", Serial: "OP5B7A9C3E",
	},
}

// HardwareFor returns the catalog entry this deviceId reports. It is a pure
// function of the deviceId, so one device keeps the same hardware across
// registrations, and distinct deviceIds spread over the catalog. It never
// varies run-to-run for the same device, the way the install values must not.
func HardwareFor(deviceID string) Hardware {
	h := fnv.New32a()
	_, _ = h.Write([]byte(deviceID))
	return hardwareCatalog[h.Sum32()%uint32(len(hardwareCatalog))]
}

// DeviceUUID returns the deterministic version-4 UUID (32 lowercase hex
// digits) this deviceId maps to, the shape of the game's
// GenerateHighVersionUUID() (UUID.randomUUID() without dashes) for the Login's
// androidDid field. Pure in deviceId, so one device keeps one UUID.
func DeviceUUID(deviceID string) string {
	sum := md5.Sum([]byte(deviceID))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return hex.EncodeToString(b)
}
