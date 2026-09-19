/**
 * 虚拟机光驱接口（F-2-06）。
 *
 * 三处界面必须表达的区分——每一条都对应一种「用户会误解而查不出来」的情况：
 *
 *   **弹出 ≠ 移除**   弹出后来宾里仍看得到一个空的托盘（设备还在），移除才是
 *                    设备消失。混为一谈的话，用户在来宾里找不到设备而不知道
 *                    是哪一种情况，而两种的排查方向完全不同。
 *   **换盘 ≠ 立刻生效** 多数系统缓存介质信息，换盘后来宾里看到的还是旧的。
 *   **换总线 ≠ 即时生效** 热插拔的总线支持各不相同，几乎一定要重启。
 */
import { del, get, post, put } from './client'

export interface CDROMView {
  id: number
  vm_id: number
  /** 光驱序号（决定来宾里的设备名）。 */
  order_no: number
  bus: 'ide' | 'sata' | 'scsi'
  /** false 表示**光驱在但没放盘**（弹出状态）——与「没有光驱」是两件事。 */
  loaded: boolean
  iso_file_id?: number
  iso_name?: string
  /** 来宾内的设备名（srvN）。**由服务端算好**，不由界面拼。 */
  device: string
  max_cdroms: number
}

export const CDROM_BUSES = [
  { value: 'sata', label: 'SATA（推荐，多数系统可热插拔）' },
  { value: 'ide', label: 'IDE（老系统；换到它通常要重启）' },
  { value: 'scsi', label: 'SCSI（需要来宾里有驱动）' },
]

export const cdromApi = {
  list: (vmID: number) => get<{ items: CDROMView[] }>(`/api/v1/vms/${vmID}/cdroms`),

  attach: (vmID: number, isoFileID: number, bus: string) =>
    post<{ task: unknown }>(`/api/v1/vms/${vmID}/cdroms`, {
      iso_file_id: isoFileID,
      bus,
    }),

  /** 放盘或换盘。**换盘后来宾通常看不到新介质**（它缓存了介质信息）。 */
  load: (vmID: number, cdromID: number, isoFileID: number) =>
    put<{ task: unknown }>(`/api/v1/vms/${vmID}/cdroms/${cdromID}/iso`, {
      iso_file_id: isoFileID,
    }),

  /** 弹出介质，**光驱保留**。 */
  eject: (vmID: number, cdromID: number) =>
    post<{ task: unknown }>(`/api/v1/vms/${vmID}/cdroms/${cdromID}/eject`, {}),

  /** 移除整个光驱。**不重排其余光驱的序号**（重排会让 sr0 与 sr1 对调）。 */
  remove: (vmID: number, cdromID: number) =>
    del<{ task: unknown }>(`/api/v1/vms/${vmID}/cdroms/${cdromID}`),

  /** 换总线类型。**几乎一定需要重启**才生效。 */
  setBus: (vmID: number, cdromID: number, bus: string) =>
    put<{ task: unknown }>(`/api/v1/vms/${vmID}/cdroms/${cdromID}/bus`, { bus }),
}
