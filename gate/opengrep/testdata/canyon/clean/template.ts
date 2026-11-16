export async function GET(req: Request, { params }: { params: { id: string } }) {
  return Response.json({ hello: `hello ${req.url}` });
}
