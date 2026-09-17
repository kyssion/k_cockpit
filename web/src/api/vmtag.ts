/**
 * 虚拟机标签接口（F-2-16）。
 *
 * 标签与分组解决的不是同一件事：分组互斥（一台机器只属于一个组），
 * 标签非互斥（可以有任意多个）。
 *
 * 保存是**整体替换**而不是增量增删——界面改完直接保存，而逐个增删会让
 * 「加了又删、删了又加」的中间态被写进审计流水，让那条记录读不懂。
 */
import { get, put } from './client'

export interface TagCount {
  tag: string
  count: number
}

export const vmTagApi = {
  list: (vmID: number) => get<{ tags: string[] }>(`/api/v1/vms/${vmID}/tags`),

  /** 整体替换。传空数组即清空。 */
  set: (vmID: number, tags: string[]) =>
    put<{ tags: string[] }>(`/api/v1/vms/${vmID}/tags`, { tags }),

  /** 全部标签及使用次数。**按调用者的可见范围**返回。 */
  all: () => get<{ items: TagCount[] }>('/api/v1/tags'),

  /**
   * 按标签查虚拟机 id。
   *
   * 不由界面自己筛：列表是分页的，前端只拿得到当前页，而「哪些机器带
   * 这个标签」必须看全量。
   */
  vmsByTag: (tag: string) => get<{ vm_ids: number[] }>('/api/v1/tags/vms', { tag }),
}
