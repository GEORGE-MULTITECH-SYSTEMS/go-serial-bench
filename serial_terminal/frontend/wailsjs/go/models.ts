export namespace main {
	
	export class PortConfig {
	    name: string;
	    baud: string;
	    dataBits: string;
	    parity: string;
	    stopBits: string;
	    handshake: string;
	    dtr: boolean;
	    rts: boolean;
	    readGapMs: string;
	    writeTimeoutMs: string;
	
	    static createFrom(source: any = {}) {
	        return new PortConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.baud = source["baud"];
	        this.dataBits = source["dataBits"];
	        this.parity = source["parity"];
	        this.stopBits = source["stopBits"];
	        this.handshake = source["handshake"];
	        this.dtr = source["dtr"];
	        this.rts = source["rts"];
	        this.readGapMs = source["readGapMs"];
	        this.writeTimeoutMs = source["writeTimeoutMs"];
	    }
	}

}

export namespace supervisor {
	
	export class State {
	    open: boolean;
	    port: string;
	    config: string;
	    dtr: boolean;
	    rts: boolean;
	    modem: Record<string, boolean>;
	    stats: winserial.Stats;
	    healthy: boolean;
	    wdAdvancing: boolean;
	    degraded: boolean;
	    autoRecycle: boolean;
	    recycles: number;
	    uptimeS: number;
	    pid: number;
	    opening: boolean;
	    present: boolean;
	    vanished: boolean;
	    wedged: boolean;
	    leakedHandles: number;
	    eventsSuppressed: number;
	    txQueued: number;
	    txDropped: number;
	
	    static createFrom(source: any = {}) {
	        return new State(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.open = source["open"];
	        this.port = source["port"];
	        this.config = source["config"];
	        this.dtr = source["dtr"];
	        this.rts = source["rts"];
	        this.modem = source["modem"];
	        this.stats = this.convertValues(source["stats"], winserial.Stats);
	        this.healthy = source["healthy"];
	        this.wdAdvancing = source["wdAdvancing"];
	        this.degraded = source["degraded"];
	        this.autoRecycle = source["autoRecycle"];
	        this.recycles = source["recycles"];
	        this.uptimeS = source["uptimeS"];
	        this.pid = source["pid"];
	        this.opening = source["opening"];
	        this.present = source["present"];
	        this.vanished = source["vanished"];
	        this.wedged = source["wedged"];
	        this.leakedHandles = source["leakedHandles"];
	        this.eventsSuppressed = source["eventsSuppressed"];
	        this.txQueued = source["txQueued"];
	        this.txDropped = source["txDropped"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace winserial {
	
	export class LineErrors {
	    Frame: number;
	    Parity: number;
	    Overrun: number;
	    RxOver: number;
	    Break: number;
	    Probes: number;
	
	    static createFrom(source: any = {}) {
	        return new LineErrors(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Frame = source["Frame"];
	        this.Parity = source["Parity"];
	        this.Overrun = source["Overrun"];
	        this.RxOver = source["RxOver"];
	        this.Break = source["Break"];
	        this.Probes = source["Probes"];
	    }
	}
	export class Stats {
	    Open: boolean;
	    Watchdog: number;
	    RxBytes: number;
	    TxBytes: number;
	    Reads: number;
	    Writes: number;
	    Errors: number;
	    ErrorStreak: number;
	    LastError: string;
	    // Go type: time
	    LastErrorAt: any;
	    Line: LineErrors;
	    ProbeErrors: number;
	    Wedged: boolean;
	    TimedOutWaits: number;
	
	    static createFrom(source: any = {}) {
	        return new Stats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Open = source["Open"];
	        this.Watchdog = source["Watchdog"];
	        this.RxBytes = source["RxBytes"];
	        this.TxBytes = source["TxBytes"];
	        this.Reads = source["Reads"];
	        this.Writes = source["Writes"];
	        this.Errors = source["Errors"];
	        this.ErrorStreak = source["ErrorStreak"];
	        this.LastError = source["LastError"];
	        this.LastErrorAt = this.convertValues(source["LastErrorAt"], null);
	        this.Line = this.convertValues(source["Line"], LineErrors);
	        this.ProbeErrors = source["ProbeErrors"];
	        this.Wedged = source["Wedged"];
	        this.TimedOutWaits = source["TimedOutWaits"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

