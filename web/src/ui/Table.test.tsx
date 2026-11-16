import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Table, TableWrap } from "./Table";

describe("Table", () => {
  it("renders inside a scrolling wrapper that carries the minimum width", () => {
    const { container } = render(
      <TableWrap minWidth={1080}>
        <Table columns={[{ key: "app", label: "App" }]} rows={["claims-portal"]} renderRow={(id) => <tr key={id}><td>{id}</td></tr>} />
      </TableWrap>,
    );
    const wrap = container.firstElementChild;
    expect(wrap?.className).toContain("overflow-x-auto");
    expect(wrap?.firstElementChild?.getAttribute("style")).toBe("min-width: 1080px;");
    expect(wrap?.querySelector("table")).not.toBeNull();
    expect(screen.getByText("claims-portal")).toBeDefined();
  });

  it("says so when there are no rows", () => {
    render(<Table columns={[{ key: "app", label: "App" }]} rows={[]} renderRow={() => null} empty="No apps yet" />);
    expect(screen.getByText("No apps yet")).toBeDefined();
  });
});
