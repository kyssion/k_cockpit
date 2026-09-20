/**
 * 语言包（FRONTEND §7）。
 *
 * **渐进迁移**，这是刻意的选择：一次性把全站几百处文案抽成键，收益是"理论
 * 上能翻译"，代价是每一处都要改一遍、每一处都可能引入笔误，而眼下真实
 * 需要的只是"界面能切换语言"这件事本身。
 *
 * 因此这里的字典只收录**通用文案**（按钮、导航、状态、主题与语言），业务
 * 页面的长句仍以中文写在页面里。t() 未命中时返回中文原文——界面不会出现
 * 空白或裸露的 key，那是比"某一句还是中文"严重得多的问题。
 */
export type Lang = 'zh-CN' | 'en-US'

const zhCN: Record<string, string> = {
  'common.search': '搜索',
  'common.confirm': '确认',
  'common.cancel': '取消',
  'common.save': '保存',
  'common.delete': '删除',
  'common.create': '创建',
  'common.edit': '修改',
  'common.close': '关闭',
  'common.prev': '上一步',
  'common.next': '下一步',
  'common.submit': '提交',
  'common.retry': '重试',
  'common.loading': '加载中…',
  'common.all': '全部',
  'common.actions': '操作',
  'common.status': '状态',
  'common.name': '名称',
  'common.node': '节点',
  'common.none': '—',

  'nav.dashboard': '工作台',
  'nav.vm': '虚拟机',
  'nav.trash': '回收站',
  'nav.alerts': '告警中心',
  'nav.task': '任务中心',
  'nav.settings': '系统设置',
  'nav.security': '安全中心',
  'nav.about': '关于项目',

  'theme.label': '主题',
  'theme.system': '跟随系统',
  'theme.light': '浅色',
  'theme.dark': '深色',

  'lang.label': '语言',
  'lang.zh-CN': '简体中文',
  'lang.en-US': 'English',

  'auth.logout': '登出',
}

const enUS: Record<string, string> = {
  'common.search': 'Search',
  'common.confirm': 'Confirm',
  'common.cancel': 'Cancel',
  'common.save': 'Save',
  'common.delete': 'Delete',
  'common.create': 'Create',
  'common.edit': 'Edit',
  'common.close': 'Close',
  'common.prev': 'Back',
  'common.next': 'Next',
  'common.submit': 'Submit',
  'common.retry': 'Retry',
  'common.loading': 'Loading…',
  'common.all': 'All',
  'common.actions': 'Actions',
  'common.status': 'Status',
  'common.name': 'Name',
  'common.node': 'Node',
  'common.none': '—',

  'nav.dashboard': 'Dashboard',
  'nav.vm': 'Virtual Machines',
  'nav.trash': 'Trash',
  'nav.alerts': 'Alerts',
  'nav.task': 'Tasks',
  'nav.settings': 'Settings',
  'nav.security': 'Security',
  'nav.about': 'About',

  'theme.label': 'Theme',
  'theme.system': 'System',
  'theme.light': 'Light',
  'theme.dark': 'Dark',

  'lang.label': 'Language',
  'lang.zh-CN': '简体中文',
  'lang.en-US': 'English',

  'auth.logout': 'Log out',
}

const dicts: Record<Lang, Record<string, string>> = {
  'zh-CN': zhCN,
  'en-US': enUS,
}

/**
 * t 取一条文案。
 *
 * 未命中时**回落到中文**再落到 key 本身，而不是返回空串：一个空白按钮比
 * "这一句还是中文"难排查得多，而且用户完全无法据此操作。
 */
export function t(key: string, lang: Lang): string {
  return dicts[lang]?.[key] ?? dicts['zh-CN']?.[key] ?? key
}

export const LANGS: { value: Lang; label: string }[] = [
  { value: 'zh-CN', label: langName('zh-CN') },
  { value: 'en-US', label: langName('en-US') },
]

/** langName 返回语言**自身的名字**（简体中文 / English），始终不翻译——
 *  语言选择器若也跟着界面语言变，不熟悉当前语言的人就再也换不回来了。 */
export function langName(lang: Lang): string {
  return lang === 'zh-CN' ? '简体中文' : 'English'
}
