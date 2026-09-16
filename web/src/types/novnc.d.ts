/**
 * noVNC 的最小类型声明。
 *
 * 该库不带 TypeScript 类型。这里**只声明我们用到的部分**，而不是从
 * DefinitelyTyped 拉一份完整声明：完整的声明会带来一堆我们用不到的类型，
 * 而它们与库的实际版本随时可能不一致——那时类型检查会给出错误的信心。
 */
declare module '@novnc/novnc' {
  export interface RFBOptions {
    credentials?: { username?: string; password?: string; target?: string }
    shared?: boolean
    repeaterID?: string
    wsProtocols?: string[]
  }

  export default class RFB {
    constructor(target: HTMLElement, url: string, options?: RFBOptions)

    /** 画面是否随容器缩放。 */
    scaleViewport: boolean
    /** 只读模式：不允许向远端发送输入。 */
    viewOnly: boolean
    /** 是否裁剪超出容器的部分（与 scaleViewport 互斥）。 */
    clipViewport: boolean
    /** 背景色（未收到画面时的填充色）。 */
    background: string

    disconnect(): void
    sendCredentials(credentials: { username?: string; password?: string }): void
    sendCtrlAltDel(): void

    addEventListener(type: string, listener: (event: Event) => void): void
    removeEventListener(type: string, listener: (event: Event) => void): void
  }
}
