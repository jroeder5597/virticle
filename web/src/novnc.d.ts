declare module "@novnc/novnc/lib/rfb" {
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, url: string, options?: { wsProtocols?: string[]; credentials?: { password?: string } });
    scaleViewport: boolean;
    resizeSession: boolean;
    background: string;
    focusOnClick: boolean;
    disconnect(): void;
    sendCtrlAltDel(): void;
    sendCredentials(c: { password: string }): void;
    focus(): void;
  }
}
