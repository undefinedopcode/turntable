// A draggable splitter bar. `dir="col"` resizes width (vertical bar, drag
// left/right); `dir="row"` resizes height (horizontal bar, drag up/down). The
// drag logic lives in the parent — this just renders the handle and forwards
// the pointer-down that begins a drag.
export function Gutter({
  dir,
  onPointerDown,
}: {
  dir: "col" | "row";
  onPointerDown: (e: React.PointerEvent) => void;
}) {
  return (
    <div
      className={`gutter gutter-${dir}`}
      role="separator"
      aria-orientation={dir === "col" ? "vertical" : "horizontal"}
      onPointerDown={onPointerDown}
    >
      <span className="gutter-grip" />
    </div>
  );
}
