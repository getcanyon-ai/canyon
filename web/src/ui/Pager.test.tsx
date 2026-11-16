import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Pager, pageList } from "./Pager";

afterEach(cleanup);

const pager = (total: number, page: number, limit = 50) => {
  const go = vi.fn();
  render(<Pager label="Pages" noun={["app", "apps"]} total={total} page={page} limit={limit} go={go} />);
  return go;
};
const buttons = () => within(screen.getByRole("navigation", { name: "Pages" })).getAllByRole("button").map((one) => one.textContent);

describe("pageList", () => {
  it("names the first, the last and the pages beside this one, an ellipsis for what it leaves out", () => {
    expect(pageList(5, 26)).toEqual([1, null, 4, 5, 6, null, 26]);
    expect(pageList(1, 26)).toEqual([1, 2, null, 26]);
    expect(pageList(26, 26)).toEqual([1, null, 25, 26]);
    expect(pageList(1, 1)).toEqual([1]);
  });

  it("shows a one-page gap as that page, never as an ellipsis standing for one page", () => {
    expect(pageList(4, 26)).toEqual([1, 2, 3, 4, 5, null, 26]);
    expect(pageList(3, 5)).toEqual([1, 2, 3, 4, 5]);
  });
});

describe("Pager", () => {
  it("says where the page sits in the server's total, and its page of all of them", () => {
    pager(1284, 1);
    expect(screen.getByText(/of 1,284 apps/).textContent).toBe("1–50 of 1,284 apps · Page 1 of 26");
  });

  it("names the last page's own last row, never a full page past the total", () => {
    pager(1284, 26);
    expect(screen.getByText(/of 1,284 apps/).textContent).toBe("1,251–1,284 of 1,284 apps · Page 26 of 26");
  });

  it("marks the current page for a screen reader and goes to exactly the page pressed", () => {
    const go = pager(1284, 5);
    expect(buttons()).toEqual(["First", "← Previous", "1", "4", "5", "6", "26", "Next →", "Last"]); // the arrows are drawn, never read out
    expect(screen.getByRole("button", { name: "Page 5" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("button", { name: "Page 4" }).hasAttribute("aria-current")).toBe(false);
    for (const [name, page] of [["First", 1], ["Previous", 4], ["Page 26", 26], ["Next", 6], ["Last", 26]] as const) {
      fireEvent.click(screen.getByRole("button", { name }));
      expect(go).toHaveBeenLastCalledWith({ page, limit: 50 });
    }
  });

  it("can't go before the first page or past the last", () => {
    pager(120, 1);
    expect((screen.getByRole("button", { name: "First" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Previous" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled).toBe(false);
    cleanup();
    pager(120, 3);
    expect((screen.getByRole("button", { name: "Last" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("changes the rows a page holds and keeps this page's first row in view", () => {
    const go = pager(1284, 5); // rows 201–250
    fireEvent.change(screen.getByRole("combobox", { name: "Rows per page" }), { target: { value: "100" } });
    expect(go).toHaveBeenLastCalledWith({ page: 3, limit: 100 }); // rows 201–300
    fireEvent.change(screen.getByRole("combobox", { name: "Rows per page" }), { target: { value: "25" } });
    expect(go).toHaveBeenLastCalledWith({ page: 9, limit: 25 }); // rows 201–225
  });

  it("offers 25, 50, 100 and 200 rows, and a size the address asked for besides", () => {
    pager(1284, 1, 10);
    expect(within(screen.getByRole("combobox", { name: "Rows per page" })).getAllByRole("option").map((one) => one.textContent)).toEqual(["10", "25", "50", "100", "200"]);
  });

  it("says one page as a count alone, with nothing to press, and says nothing when there are no rows", () => {
    pager(7, 1);
    expect(screen.getByText(/of 7 apps/).textContent).toBe("1–7 of 7 apps");
    expect(screen.queryAllByRole("button")).toEqual([]);
    expect(screen.queryByRole("combobox")).toBeNull();
    cleanup();
    pager(0, 1);
    expect(screen.queryByRole("navigation")).toBeNull();
  });
});
