# A Streamlit report — the shape a lot of non-developer apps take: an
# analyst's script that grew a UI.
#
# Streamlit is its own server with its own habits. Left alone it picks port
# 8501, binds all interfaces only if told, and serves a websocket the page
# needs. Nothing here declares a port, a start command or a health route, and
# the code has no `if __name__ == "__main__"` at all — it is run *by* the
# streamlit binary, not by python.

import pandas as pd
import streamlit as st

st.set_page_config(page_title="Quarterly report", page_icon="📈", layout="centered")

st.title("Quarterly report")
st.write(
    "An app written by someone who would not call themselves a developer: "
    "no port, no start command, no health route, no Dockerfile."
)

data = pd.DataFrame(
    {
        "quarter": ["Q1", "Q2", "Q3", "Q4"],
        "revenue": [128_000, 141_500, 139_200, 172_800],
    }
)

st.metric("Best quarter", "Q4", "+24% vs Q3")
st.bar_chart(data, x="quarter", y="revenue")

with st.expander("The numbers"):
    st.dataframe(data, hide_index=True, width="stretch")

st.caption(
    "Deployed with donkey. Streamlit needs --server.port $PORT and "
    "--server.address 0.0.0.0 to be reachable in a container."
)
