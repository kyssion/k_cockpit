package agent

// OpVMFloppyApply 挂载或弹出虚拟机的软盘。
//
// 软盘是**独立于光驱**的一类设备：有些老系统的安装流程或驱动盘仍走软盘，
// 而它与光驱在来宾里是两个不同的设备，不能共用一个"可移动介质"抽象——
// 那样会让"这台机器挂了什么"有两处答案。
const OpVMFloppyApply OpKind = "vm.floppy.apply"
