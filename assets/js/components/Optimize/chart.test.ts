import { describe, expect, test } from "vite-plus/test";
import { disconnectedSlots, expandSlots, isMidnight, netGridCost, slotXAxis } from "./chart";

// 15min slots starting at the given local hour
function times(startHour: number, count: number): number[] {
  const start = new Date(2025, 0, 1, startHour).getTime();
  return Array.from({ length: count }, (_, i) => start + i * 15 * 60 * 1000);
}

describe("slotXAxis", () => {
  const weekdayShort = () => "Thu";
  // 20:00 to 08:00, midnight at index 16
  const slots = times(20, 48);
  const axis = slotXAxis(slots, weekdayShort);
  const label = (i: number) => axis.axisLabel.formatter(String(slots[i]));

  test("labels full hours only", () => {
    expect(label(0)).toBe("20"); // 20:00
    expect(label(1)).toBe(""); // 20:15
    expect(label(4)).toBe(""); // 21:00, not a step hour
    expect(label(32)).toBe("4"); // 04:00
  });

  test("shows the weekday at midnight", () => {
    expect(label(16)).toBe("0\nThu");
  });

  test("draws a split line at day boundaries only", () => {
    expect(isMidnight(slots[16])).toBe(true);
    expect(axis.splitLine.interval(16)).toBe(true);
    expect(axis.splitLine.interval(15)).toBe(false);
    // no line at the axis start, even if the first slot is midnight
    expect(slotXAxis(times(0, 4), weekdayShort).splitLine.interval(0)).toBe(false);
  });
});

describe("disconnectedSlots", () => {
  test("flags slots where the per-slot charge cap is 0", () => {
    expect(disconnectedSlots([8000, 0, 0, 8000], 4)).toEqual([false, true, true, false]);
  });

  test("no c_max_t (e.g. home battery) is never disconnected", () => {
    expect(disconnectedSlots(undefined, 3)).toEqual([false, false, false]);
  });

  test("pads to the requested length when c_max_t is shorter", () => {
    expect(disconnectedSlots([0], 3)).toEqual([true, false, false]);
  });
});

describe("expandSlots", () => {
  const t = [0, 100, 200];
  const endTime = 300;

  test("emits two points per slot forming a staircase", () => {
    expect(expandSlots(t, endTime, [1, 2, 3])).toEqual([
      [0, 1],
      [100, 1],
      [100, 2],
      [200, 2],
      [200, 3],
      [300, 3],
    ]);
  });

  test("keeps null slots as gaps so a masked layer breaks cleanly", () => {
    // the middle slot is masked out; its two points stay null so the line does not
    // bridge the connected slots on either side
    expect(expandSlots(t, endTime, [1, null, 3])).toEqual([
      [0, 1],
      [100, 1],
      [100, null],
      [200, null],
      [200, 3],
      [300, 3],
    ]);
  });
});

describe("netGridCost", () => {
  test("import cost minus export revenue, per step prices", () => {
    // 1 kWh bought at 0.30, 2 kWh sold at 0.10 then 0.05
    const cost = netGridCost([1000, 0], [0, 2000], [0.0003, 0.0003], [0.0001, 0.00005]);
    expect(cost).toBeCloseTo(0.3 - 0.1);
  });

  test("no import and some export is a credit", () => {
    expect(netGridCost([0, 0], [1000, 1000], [0.0003, 0.0003], [0.0001, 0.0001])).toBeCloseTo(-0.2);
  });

  test("missing series count as nothing", () => {
    expect(netGridCost(undefined, undefined, undefined, undefined)).toBe(0);
    expect(netGridCost([1000], [1000], undefined, undefined)).toBe(0);
  });
});
